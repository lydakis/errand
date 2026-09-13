package manifest

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestMaterializedUpdatesOwnConcurrentExports(t *testing.T) {
	entries := make([]proto.ManifestEntry, minIndexedEntries)
	for i := range entries {
		entries[i] = file(fmt.Sprintf("file-%04d", i), 1)
	}
	base := mustNew(t, entries)
	first, err := base.Update(t.Context(), []Edit{{Entry: file(entries[0].Path, 2)}})
	if err != nil {
		t.Fatal(err)
	}
	// No export between these updates: second must materialize from its tree.
	second, err := first.Update(t.Context(), []Edit{{Entry: file(entries[1].Path, 3)}})
	if err != nil {
		t.Fatal(err)
	}
	for i, state := range []*Snapshot{first, second} {
		want := slices.Clone(entries)
		want[0].Size = 2
		want[0].SHA256 = file("", 2).SHA256
		if i == 1 {
			want[1].Size = 3
			want[1].SHA256 = file("", 3).SHA256
		}
		wantRoot := (proto.Manifest{Entries: want}).RootHash()
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				m, err := state.Manifest(t.Context())
				if err != nil || !slices.Equal(m.Entries, want) {
					t.Errorf("export: %v", err)
					return
				}
				m.Entries[0].Path = "caller-mutation"
				root, err := state.RootHash(t.Context())
				if err != nil || root != wantRoot {
					t.Errorf("shared export changed identity: %v", err)
				}
			})
		}
		wg.Wait()
	}
}
