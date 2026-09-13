// Package snapshotindex compares immutable metadata representations. It is an
// experiment, not a production snapshot API. Inputs are already validated,
// canonical manifests; selection policy and filesystem evidence live outside it.
package snapshotindex

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"

	"github.com/lydakis/errand/internal/proto"
)

// Edit replaces one exact path, or removes it when Delete is true. Removing a
// directory requires explicit edits for its descendants, as produced by Diff.
// Inputs contain unique paths. Entry.Path is also the key for deletions.
type Edit struct {
	Entry  proto.ManifestEntry
	Delete bool
}

// Index owns its metadata. Update and Manifest must not alias caller slices.
// Diff returns sorted edits that transform the receiver into next. Implementations
// only compare their own type. Digest is representation-specific, deterministic
// for the same entries, and is NOT the current protocol's Manifest.RootHash.
type Index interface {
	Update([]Edit) Index
	Diff(next Index) []Edit
	Manifest() proto.Manifest
	Digest() [32]byte
}

func entryDigest(entry proto.ManifestEntry) [32]byte {
	// Length-prefixed fields preserve exact boundaries without allocating JSON.
	// Common entries fit on the stack; longer paths/targets grow normally. This
	// is an experimental identity encoding, not the protocol's Manifest.RootHash.
	var storage [512]byte
	data := append(storage[:0], 1) // Entry encoding version/domain.
	for _, value := range []string{entry.Path, entry.Type, entry.SHA256, entry.Target} {
		data = binary.LittleEndian.AppendUint64(data, uint64(len(value)))
		data = append(data, value...)
	}
	data = binary.LittleEndian.AppendUint32(data, entry.Mode)
	data = binary.LittleEndian.AppendUint64(data, uint64(entry.Size))
	return sha256.Sum256(data)
}

// diffEntries is the flat, ordered reference implementation.
func diffEntries(before, after []proto.ManifestEntry) []Edit {
	var edits []Edit
	i, j := 0, 0
	for i < len(before) || j < len(after) {
		switch {
		case j == len(after) || i < len(before) && before[i].Path < after[j].Path:
			edits = append(edits, Edit{Entry: before[i], Delete: true})
			i++
		case i == len(before) || after[j].Path < before[i].Path:
			edits = append(edits, Edit{Entry: after[j]})
			j++
		default:
			if before[i] != after[j] {
				edits = append(edits, Edit{Entry: after[j]})
			}
			i++
			j++
		}
	}
	return edits
}

func sortedEdits(edits []Edit) []Edit {
	result := append([]Edit(nil), edits...)
	sort.Slice(result, func(i, j int) bool { return result[i].Entry.Path < result[j].Entry.Path })
	return result
}
