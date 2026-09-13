package archive

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestSortedValidationMatchesArchiveHierarchyRules(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 19))
	paths := []string{"a", "a-", "a-1", "a-1/child", "a/b", "a/b/c", "a/c", "a0", "b", "b/a", "b/a/child", "b/other", "z"}
	for range 300 {
		var entries []proto.ManifestEntry
		for _, name := range paths {
			switch rng.IntN(4) {
			case 0:
				entries = append(entries, entryFile(name, "data"))
			case 1:
				entries = append(entries, proto.ManifestEntry{Path: name, Type: proto.EntryDir, Mode: 0755})
			case 2:
				entries = append(entries, proto.ManifestEntry{Path: name, Type: proto.EntrySymlink, Target: "z"})
			}
		}
		slices.SortFunc(entries, func(a, b proto.ManifestEntry) int { return strings.Compare(a.Path, b.Path) })
		m := proto.Manifest{Entries: entries}
		ordinary, sorted := Validate(m), ValidateSortedContext(t.Context(), m)
		if (ordinary == nil) != (sorted == nil) {
			t.Fatalf("validation differs for %+v: ordinary=%v sorted=%v", entries, ordinary, sorted)
		}
	}
}
