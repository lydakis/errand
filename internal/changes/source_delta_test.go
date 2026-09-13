package changes

import (
	"context"
	"errors"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestSourceDeltaReconstructionAndPartialAcceptance(t *testing.T) {
	base := proto.Manifest{Entries: []proto.ManifestEntry{
		{Path: "dir", Type: proto.EntryDir, Mode: 0755},
		{Path: "dir/old", Type: proto.EntrySymlink, Mode: 0777, Target: "../target"},
		{Path: "target", Type: proto.EntryDir, Mode: 0755},
	}}
	current := proto.Manifest{Entries: []proto.ManifestEntry{
		{Path: "dir", Type: proto.EntryDir, Mode: 0700},
		{Path: "dir/new", Type: proto.EntrySymlink, Mode: 0777, Target: "../target"},
		{Path: "target", Type: proto.EntryDir, Mode: 0755},
	}}
	delta, err := PrepareSourceDelta(context.Background(), base, current, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ExpandSourceDelta(base, delta, current.RootHash(), 1<<20)
	if err != nil || got.RootHash() != current.RootHash() {
		t.Fatalf("reconstruct: %+v %v", got, err)
	}
	partial, err := AcceptedSource(base, current, []string{"dir"})
	if err != nil {
		t.Fatal(err)
	}
	if partial.Entries[0].Mode != 0700 || partial.Entries[1].Path != "dir/old" {
		t.Fatalf("metadata apply adopted child changes: %+v", partial)
	}
	for _, selected := range [][]string{nil, {"dir"}, delta.Paths} {
		want, err := AcceptedSource(base, current, selected)
		if err != nil {
			t.Fatal(err)
		}
		got, err := AcceptedSourceDelta(t.Context(), base, delta, selected)
		if err != nil || got.RootHash() != want.RootHash() {
			t.Fatalf("delta acceptance differs: %+v %v", got, err)
		}
	}
	if _, err := AcceptedSourceDelta(t.Context(), current, delta, delta.Paths); !errors.Is(err, ErrCheckpointChanged) {
		t.Fatalf("accepted stale base: %v", err)
	}
	if _, err := AcceptedSourceDelta(t.Context(), base, delta, []string{"unknown"}); err == nil {
		t.Fatal("accepted unknown receipt path")
	}
	for _, mutate := range []func(*proto.ChangeBundle){
		func(b *proto.ChangeBundle) { b.BaselineRoot = "wrong" },
		func(b *proto.ChangeBundle) { b.Paths = b.Paths[:1] },
		func(b *proto.ChangeBundle) { b.MetadataPaths = nil },
		func(b *proto.ChangeBundle) { b.BaseManifest = proto.Manifest{} },
	} {
		bad := delta
		mutate(&bad)
		if _, err := ExpandSourceDelta(base, bad, current.RootHash(), 1<<20); err == nil {
			t.Fatalf("accepted damaged delta: %+v", bad)
		}
	}
}
