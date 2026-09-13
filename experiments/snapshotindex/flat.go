package snapshotindex

import (
	"crypto/sha256"
	"encoding/json"

	"github.com/lydakis/errand/internal/proto"
)

type flat struct {
	entries []proto.ManifestEntry
	digest  [32]byte
}

// NewFlat models the existing sorted manifest and full JSON root hashing.
func NewFlat(entries []proto.ManifestEntry) Index {
	return newFlatOwned(append([]proto.ManifestEntry(nil), entries...))
}
func newFlatOwned(entries []proto.ManifestEntry) Index {
	if len(entries) == 0 {
		entries = nil
	}
	f := &flat{entries: entries}
	data, err := json.Marshal(proto.Manifest{Entries: f.entries})
	if err != nil {
		panic(err)
	}
	f.digest = sha256.Sum256(data)
	return f
}

func (f *flat) Update(edits []Edit) Index {
	if len(edits) == 0 {
		return f
	}
	return newFlatOwned(editEntries(f.entries, sortedEdits(edits)))
}
func (f *flat) Diff(next Index) []Edit {
	n := next.(*flat)
	if f.digest == n.digest {
		return nil
	}
	return diffEntries(f.entries, n.entries)
}
func (f *flat) Manifest() proto.Manifest {
	return proto.Manifest{Entries: append([]proto.ManifestEntry(nil), f.entries...)}
}
func (f *flat) Digest() [32]byte { return f.digest }

// Both arguments are sorted. The returned slice owns its entries.
func editEntries(entries []proto.ManifestEntry, edits []Edit) []proto.ManifestEntry {
	result := make([]proto.ManifestEntry, 0, len(entries)+len(edits))
	i, j := 0, 0
	for i < len(entries) || j < len(edits) {
		if j == len(edits) || i < len(entries) && entries[i].Path < edits[j].Entry.Path {
			result = append(result, entries[i])
			i++
			continue
		}
		if i < len(entries) && entries[i].Path == edits[j].Entry.Path {
			i++
		}
		if !edits[j].Delete {
			result = append(result, edits[j].Entry)
		}
		j++
	}
	return result
}
