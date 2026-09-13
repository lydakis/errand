package changes

import (
	"errors"
	"reflect"
	"testing"

	"github.com/lydakis/errand/internal/manifest"
	"github.com/lydakis/errand/internal/proto"
)

func TestRetainedSourceMatchesCheckpointAdvancement(t *testing.T) {
	base := observationManifest(map[string]string{"dir/a": "before", "dir/b": "keep", "other": "old"})
	base.Entries = append([]proto.ManifestEntry{{Path: "dir", Type: proto.EntryDir, Mode: 0755}}, base.Entries...)
	cases := []proto.Manifest{
		observationManifest(map[string]string{"dir": "replacement", "other": "new"}),
		observationManifest(map[string]string{"dir/a": "after", "dir/new": "added", "other": "new"}),
		{},
		{Entries: []proto.ManifestEntry{}},
	}
	metadata := cloneSourceManifest(base)
	metadata.Entries[0].Mode = 0700
	metadata.Entries[1].Mode = 0600
	cases = append(cases, metadata, base)
	for _, current := range cases {
		before, err := manifest.New(t.Context(), base)
		if err != nil {
			t.Fatal(err)
		}
		if err := before.PrepareUpdates(t.Context()); err != nil {
			t.Fatal(err)
		}
		after, err := manifest.New(t.Context(), current)
		if err != nil {
			t.Fatal(err)
		}
		if err := after.PrepareUpdates(t.Context()); err != nil {
			t.Fatal(err)
		}
		plan, err := PrepareSnapshotDelta(t.Context(), before, after, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		delta := plan.Bundle()
		cold, err := PrepareSourceDelta(t.Context(), base, current, 1<<20)
		if err != nil || !reflect.DeepEqual(delta, cold) {
			t.Fatalf("retained delta differs: %v", err)
		}
		selections := [][]string{nil, delta.Paths}
		for _, name := range delta.Paths {
			selections = append(selections, []string{name})
		}
		for _, selected := range selections {
			want, err := AcceptedSourceDelta(t.Context(), base, delta, selected)
			if err != nil {
				t.Fatal(err)
			}
			got, err := plan.Accepted(t.Context(), selected)
			if err != nil {
				t.Fatal(err)
			}
			m, err := got.Manifest(t.Context())
			if err != nil || !reflect.DeepEqual(m, want) {
				t.Fatalf("selection %v: got %v want %v: %v", selected, m, want, err)
			}
			root, err := before.RootHash(t.Context())
			if err != nil || root != base.RootHash() {
				t.Fatal("mutated prior checkpoint")
			}
		}
		if _, err := AcceptedSnapshotDelta(t.Context(), before, delta, []string{"unknown"}); err == nil {
			t.Fatal("accepted unknown receipt path")
		}
		stale := cloneSourceDelta(delta)
		stale.BaselineRoot = current.RootHash()
		if stale.BaselineRoot != base.RootHash() {
			if _, err := AcceptedSnapshotDelta(t.Context(), before, stale, delta.Paths); !errors.Is(err, ErrCheckpointChanged) {
				t.Fatalf("stale checkpoint: %v", err)
			}
		}
		if len(delta.BaseManifest.Entries) > 0 {
			forged := cloneSourceDelta(delta)
			forged.BaseManifest.Entries[0].Mode ^= 0100
			if _, err := AcceptedSnapshotDelta(t.Context(), before, forged, nil); err == nil {
				t.Fatal("accepted forged merge base")
			}
		}
	}
}
