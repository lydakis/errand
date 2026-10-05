package fslink

import (
	"reflect"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestManifestDirectoryLinks(t *testing.T) {
	m := proto.Manifest{Entries: []proto.ManifestEntry{
		{Path: "z", Type: proto.EntrySymlink, Target: "target"},
		{Path: "target", Type: proto.EntryDir},
		{Path: "target/nested/file", Type: proto.EntryFile},
		{Path: "a", Type: proto.EntrySymlink, Target: "z"},
		{Path: "loop", Type: proto.EntrySymlink, Target: "loop"},
	}}
	original := append([]proto.ManifestEntry(nil), m.Entries...)
	lookup := ManifestLookup(m)
	for _, tc := range []struct {
		target string
		dir    bool
	}{
		{target: "target", dir: true},
		{target: "a", dir: true},
		{target: "a/nested", dir: true},
		{target: "target/nested", dir: true},
		{target: "a/nested/file"},
		{target: "missing"},
		{target: "loop"},
		{target: "../outside"},
		{target: ".", dir: true},
	} {
		t.Run(tc.target, func(t *testing.T) {
			if got := IsDirectory(proto.ManifestEntry{Path: "link", Target: tc.target}, lookup); got != tc.dir {
				t.Fatalf("directory = %v, want %v", got, tc.dir)
			}
		})
	}
	if !reflect.DeepEqual(m.Entries, original) {
		t.Fatal("lookup reordered the borrowed manifest")
	}
}
