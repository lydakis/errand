package manifest

import (
	"fmt"
	"slices"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestSnapshotAcrossIndexCutoff(t *testing.T) {
	entries := make([]proto.ManifestEntry, minIndexedEntries-1)
	for i := range entries {
		entries[i] = file(fmt.Sprintf("file-%05d", i), 1)
	}
	current := mustNew(t, entries)
	if err := current.PrepareUpdates(t.Context()); err != nil {
		t.Fatal(err)
	}
	if current.index.Load() != nil {
		t.Fatal("small inventory paid for an unused index")
	}
	for _, edit := range []Edit{
		{Entry: file(entries[0].Path, 2)},
		{Entry: file(fmt.Sprintf("file-%05d", minIndexedEntries-1), 3)},
		{Entry: file(fmt.Sprintf("file-%05d", minIndexedEntries-1), 3), Delete: true},
	} {
		previous := export(t, current)
		next, err := current.Update(t.Context(), []Edit{edit})
		if err != nil {
			t.Fatal(err)
		}
		want := slices.Clone(previous.Entries)
		if edit.Entry.Path == want[0].Path {
			want[0] = edit.Entry
		} else if edit.Delete {
			want = want[:len(want)-1]
		} else {
			want = append(want, edit.Entry)
		}
		got := export(t, next)
		root, err := next.RootHash(t.Context())
		if err != nil || !slices.Equal(got.Entries, want) || root != (proto.Manifest{Entries: want}).RootHash() {
			t.Fatal("cutoff changed snapshot contents")
		}
		if !slices.Equal(export(t, current).Entries, previous.Entries) {
			t.Fatal("cutoff update mutated prior state")
		}
		if len(want) >= minIndexedEntries && next.index.Load() == nil {
			t.Fatal("large retained inventory did not build its index")
		}
		current = next
	}
}
