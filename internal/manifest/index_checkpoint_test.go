package manifest

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestIndexCheckpointRoundTripAndUpdates(t *testing.T) {
	for _, count := range []int{0, 17, minIndexedEntries, 10000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			entries := make([]proto.ManifestEntry, count)
			for i := range entries {
				entries[i] = file(fmt.Sprintf("f%05d", i), 1)
			}
			original := mustNew(t, entries)
			image, err := original.CheckpointIndex(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			restored, err := RestoreIndex(t.Context(), export(t, original), image)
			if err != nil {
				t.Fatal(err)
			}
			if (restored.index.Load() != nil) != (count >= minIndexedEntries) {
				t.Fatal("changed selected representation")
			}
			if diff, err := original.Diff(t.Context(), restored); err != nil || len(diff) != 0 {
				t.Fatalf("round-trip diff: %v %v", diff, err)
			}
			edits := []Edit{{Entry: file("f00000", 2)}, {Entry: file("z", 3)}}
			if count > 17 {
				edits = append(edits, Edit{Entry: entries[17], Delete: true})
			}
			a, err := original.Update(t.Context(), edits)
			if err != nil {
				t.Fatal(err)
			}
			b, err := restored.Update(t.Context(), edits)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(export(t, a).Entries, export(t, b).Entries) {
				t.Fatal("restored update differs")
			}
			want := []Edit{{Entry: file("f00000", 2)}}
			if count > 17 {
				want = append(want, Edit{Entry: entries[17], Delete: true})
			}
			want = append(want, Edit{Entry: file("z", 3)})
			diff, err := original.Diff(t.Context(), b)
			if err != nil || !slices.Equal(diff, want) {
				t.Fatalf("diff: %v %v", diff, err)
			}
			if diff, err := b.Diff(t.Context(), a); err != nil || len(diff) != 0 {
				t.Fatalf("equivalent updates differ: %v %v", diff, err)
			}
			if count > 0 {
				entries[0].Path = "mutated"
			}
			if !slices.Equal(export(t, original).Entries, export(t, restored).Entries) {
				t.Fatal("mutable restored state")
			}
		})
	}
}

func TestIndexCheckpointRejectsCorruptDerivedState(t *testing.T) {
	entries := make([]proto.ManifestEntry, minIndexedEntries)
	for i := range entries {
		entries[i] = file(fmt.Sprintf("f%05d", i), 1)
	}
	s := mustNew(t, entries)
	image, err := s.CheckpointIndex(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for name, damage := range map[string]func(*IndexCheckpoint){
		"version":      func(c *IndexCheckpoint) { c.Version++ },
		"missing":      func(c *IndexCheckpoint) { c.Nodes = nil },
		"digest":       func(c *IndexCheckpoint) { c.Nodes[0].Digest[0] ^= 1 },
		"content":      func(c *IndexCheckpoint) { c.Nodes[0].Content[0] ^= 1 },
		"cycle":        func(c *IndexCheckpoint) { c.Nodes[0].Left = 0 },
		"range":        func(c *IndexCheckpoint) { c.Nodes[0].Entry = len(entries) },
		"disconnected": func(c *IndexCheckpoint) { c.Nodes[len(c.Nodes)-1].Left = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := image
			bad.Nodes = slices.Clone(image.Nodes)
			damage(&bad)
			if _, err := RestoreIndex(t.Context(), export(t, s), bad); err == nil {
				t.Fatal("accepted corrupt index")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := RestoreIndex(ctx, export(t, s), image); err != context.Canceled {
		t.Fatal(err)
	}
}
