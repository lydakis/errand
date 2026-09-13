package changes

import (
	"slices"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestSubtreeManifestSortedPrefixBoundaries(t *testing.T) {
	names := []string{"a", "a-b", "a.b", "a/b", "a/b-c", "a/b/c", "a0", "z", "z/child"}
	manifest := proto.Manifest{}
	for _, name := range names {
		manifest.Entries = append(manifest.Entries, proto.ManifestEntry{Path: name})
	}
	for _, root := range []string{"a", "a/b", "a.b", "absent", "z", "zz"} {
		var want []proto.ManifestEntry
		for _, e := range manifest.Entries {
			if e.Path == root || strings.HasPrefix(e.Path, root+"/") {
				want = append(want, e)
			}
		}
		if got := subtreeManifest(manifest, root); !slices.Equal(got.Entries, want) {
			t.Fatalf("root=%q got=%v want=%v", root, got.Entries, want)
		}
	}
}
