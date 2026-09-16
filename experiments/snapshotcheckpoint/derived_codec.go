//go:build darwin || linux

package snapshotcheckpoint

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"io"

	"github.com/lydakis/errand/internal/manifest"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

type derivedHeader struct {
	Identity                        identity
	Delta                           bool
	Count, IndexCount, IndexVersion int
	Fallback                        bool
}
type observationEdit struct {
	Value  observation
	Delete bool
}

// Limit allocation while encoding, not after a potentially oversized gob exists.
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > maxBytes-b.Len() {
		return 0, fmt.Errorf("checkpoint byte limit")
	}
	return b.Buffer.Write(p)
}

func (s framedStore) encode(ctx context.Context, key identity, entries snapshot.VerifiedObservations, state *manifest.Snapshot, prior loadedCheckpoint, delta bool) ([]byte, error) {
	if !entries.Valid() || entries.Root() != key.Root || entries.Len() > maxEntries {
		return nil, fmt.Errorf("verified checkpoint observations required")
	}
	var edits []observationEdit
	var index manifest.IndexCheckpoint
	var err error
	h := derivedHeader{Identity: key, Delta: delta, Count: entries.Len()}
	if delta {
		edits, err = observationDifferences(ctx, prior.Entries, entries)
		h.Count = len(edits)
	} else if s.index {
		index, err = state.CheckpointIndex(ctx)
		h.IndexCount, h.IndexVersion, h.Fallback = len(index.Nodes), index.Version, index.Fallback
	}
	if err != nil {
		return nil, err
	}
	var buffer limitedBuffer
	e := gob.NewEncoder(&buffer)
	if err := e.Encode(h); err != nil {
		return nil, err
	}
	// Concrete bounded batches preserve immutable verified ownership without a
	// full observation copy. Gob and cancellation work are bounded per batch.
	var batch []observation
	if !delta {
		batch = make([]observation, min(codecBatch, h.Count))
	}
	for start := 0; start < h.Count; start += codecBatch {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(start+codecBatch, h.Count)
		if delta {
			err = e.Encode(edits[start:end])
		} else {
			batch = batch[:end-start]
			for i := range batch {
				batch[i] = entries.At(start + i)
			}
			err = e.Encode(batch)
		}
		if err != nil {
			return nil, err
		}
	}
	for start := 0; start < len(index.Nodes); start += codecBatch {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := e.Encode(index.Nodes[start:min(start+codecBatch, len(index.Nodes))]); err != nil {
			return nil, err
		}
	}
	return buffer.Bytes(), ctx.Err()
}

func observationDifferences(ctx context.Context, before []observation, after snapshot.VerifiedObservations) ([]observationEdit, error) {
	var edits []observationEdit
	i, j := 0, 0
	for i < len(before) || j < after.Len() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var b observation
		if j < after.Len() {
			b = after.At(j)
		}
		switch {
		case j == after.Len() || i < len(before) && before[i].Entry.Path < b.Entry.Path:
			edits = append(edits, observationEdit{before[i], true})
			i++
		case i == len(before) || b.Entry.Path < before[i].Entry.Path:
			edits = append(edits, observationEdit{Value: b})
			j++
		default:
			if before[i] != b {
				edits = append(edits, observationEdit{Value: b})
			}
			i++
			j++
		}
	}
	return edits, nil
}

func (s framedStore) decode(ctx context.Context, payload []byte, key identity, prior loadedCheckpoint, delta bool) (loadedCheckpoint, error) {
	d := gob.NewDecoder(contextReader{ctx, bytes.NewReader(payload)})
	var h derivedHeader
	if err := d.Decode(&h); err != nil {
		return loadedCheckpoint{}, err
	}
	if h.Identity != key || h.Delta != delta || h.Count < 0 || h.Count > maxEntries*2 || h.IndexCount < 0 || h.IndexCount > maxEntries {
		return loadedCheckpoint{}, fmt.Errorf("invalid derived checkpoint header")
	}
	if !delta && h.Count > maxEntries || delta && (h.IndexCount != 0 || h.IndexVersion != 0 || h.Fallback) {
		return loadedCheckpoint{}, fmt.Errorf("invalid derived checkpoint counts")
	}
	if !s.index && (h.IndexCount != 0 || h.IndexVersion != 0 || h.Fallback) {
		return loadedCheckpoint{}, fmt.Errorf("derived state in observation-only checkpoint")
	}
	var entries []observation
	var edits []observationEdit
	if delta {
		edits = make([]observationEdit, 0, h.Count)
	} else {
		entries = make([]observation, 0, h.Count)
	}
	for start := 0; start < h.Count; start += codecBatch {
		if err := ctx.Err(); err != nil {
			return loadedCheckpoint{}, err
		}
		count := min(codecBatch, h.Count-start)
		if delta {
			var batch []observationEdit
			if err := d.Decode(&batch); err != nil {
				return loadedCheckpoint{}, err
			}
			if len(batch) != count {
				return loadedCheckpoint{}, fmt.Errorf("invalid delta batch")
			}
			edits = append(edits, batch...)
		} else {
			var batch []observation
			if err := d.Decode(&batch); err != nil {
				return loadedCheckpoint{}, err
			}
			if len(batch) != count {
				return loadedCheckpoint{}, fmt.Errorf("invalid observation batch")
			}
			entries = append(entries, batch...)
		}
	}
	index := manifest.IndexCheckpoint{Version: h.IndexVersion, Fallback: h.Fallback, Nodes: make([]manifest.IndexRecord, 0, h.IndexCount)}
	for len(index.Nodes) < h.IndexCount {
		if err := ctx.Err(); err != nil {
			return loadedCheckpoint{}, err
		}
		var batch []manifest.IndexRecord
		if err := d.Decode(&batch); err != nil {
			return loadedCheckpoint{}, err
		}
		if len(batch) != min(codecBatch, h.IndexCount-len(index.Nodes)) {
			return loadedCheckpoint{}, fmt.Errorf("invalid index batch")
		}
		index.Nodes = append(index.Nodes, batch...)
	}
	var trailing derivedHeader
	if err := d.Decode(&trailing); err != io.EOF {
		return loadedCheckpoint{}, fmt.Errorf("trailing derived checkpoint data: %v", err)
	}
	var state *manifest.Snapshot
	var err error
	if delta {
		// Preserve the base observation array while replaying validated index
		// updates. The reader coalesces stamp/entry edits and copies it only once.
		entries = prior.Entries
		changes := make([]manifest.Edit, 0, len(edits))
		for i, e := range edits {
			if i > 0 && edits[i-1].Value.Entry.Path >= e.Value.Entry.Path {
				return loadedCheckpoint{}, fmt.Errorf("unordered observation edits")
			}
			if e.Delete {
				if _, ok := prior.state.Lookup(e.Value.Entry.Path); !ok {
					return loadedCheckpoint{}, fmt.Errorf("missing deleted observation")
				}
			}
			changes = append(changes, manifest.Edit{Entry: e.Value.Entry, Delete: e.Delete})
		}
		state, err = prior.state.Update(ctx, changes)
	} else {
		m := proto.Manifest{Entries: make([]proto.ManifestEntry, len(entries))}
		for i := range entries {
			m.Entries[i] = entries[i].Entry
		}
		if s.index {
			state, err = manifest.RestoreIndex(ctx, m, index)
		} else {
			state, err = manifest.New(ctx, m)
		}
	}
	if err != nil {
		return loadedCheckpoint{}, err
	}
	if state.Len() > maxEntries {
		return loadedCheckpoint{}, fmt.Errorf("checkpoint entry limit")
	}
	return loadedCheckpoint{checkpoint: checkpoint{key, entries}, state: state, pendingEdits: edits}, ctx.Err()
}

func applyObservations(ctx context.Context, before []observation, edits []observationEdit) ([]observation, error) {
	result := make([]observation, 0, len(before))
	i := 0
	for j, e := range edits {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := e.Value.Entry.Path
		if j > 0 && edits[j-1].Value.Entry.Path >= name {
			return nil, fmt.Errorf("unordered observation edits")
		}
		for i < len(before) && before[i].Entry.Path < name {
			result = append(result, before[i])
			i++
		}
		if i < len(before) && before[i].Entry.Path == name {
			i++
		} else if e.Delete {
			return nil, fmt.Errorf("missing deleted observation")
		}
		if !e.Delete {
			result = append(result, e.Value)
		}
	}
	result = append(result, before[i:]...)
	if len(result) > maxEntries {
		return nil, fmt.Errorf("checkpoint entry limit")
	}
	return result, ctx.Err()
}
