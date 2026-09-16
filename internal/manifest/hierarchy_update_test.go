package manifest

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

// Compare incremental validation with a freshly validated complete manifest,
// including batches that look like replacements but change hierarchy semantics.
func TestUpdateHierarchyPreservation(t *testing.T) {
	base := []proto.ManifestEntry{
		{Path: "a", Type: proto.EntryDir, Mode: 0755}, file("a/b", 1),
		{Path: "link", Type: proto.EntrySymlink, Target: "a/b"}, file("z", 1),
	}
	for _, representation := range []string{"flat", "tree", "fallback"} {
		for name, edits := range map[string][]Edit{
			"metadata-single":       {{Entry: file("a/b", 4)}},
			"link-to-file":          {{Entry: file("link", 3)}},
			"metadata":              {{Entry: proto.ManifestEntry{Path: "a", Type: proto.EntryDir, Mode: 0700}}, {Entry: file("a/b", 4)}, {Entry: proto.ManifestEntry{Path: "link", Type: proto.EntrySymlink, Target: "z"}}},
			"parent-type":           {{Entry: file("a", 2)}, {Entry: file("z", 4)}},
			"implicit-parent":       {{Entry: file("a/b/c", 2)}, {Entry: file("z", 4)}},
			"remove-descendant":     {{Entry: file("a", 2)}, {Entry: file("a/b", 0), Delete: true}},
			"add-directory":         {{Entry: proto.ManifestEntry{Path: "z", Type: proto.EntryDir}}, {Entry: file("z/child", 2)}},
			"invalid-file-metadata": {{Entry: file("a/b", -1)}},
			"invalid-link-target":   {{Entry: proto.ManifestEntry{Path: "link", Type: proto.EntrySymlink, Target: "../escape"}}},
		} {
			t.Run(representation+"/"+name, func(t *testing.T) {
				s := mustNew(t, base)
				if representation == "tree" {
					if _, err := s.tree(t.Context()); err != nil {
						t.Fatal(err)
					}
				} else if representation == "fallback" {
					s.noIndex.Store(true)
				}
				wanted := map[string]proto.ManifestEntry{}
				for _, e := range base {
					wanted[e.Path] = e
				}
				for _, e := range edits {
					if e.Delete {
						delete(wanted, e.Entry.Path)
					} else {
						wanted[e.Entry.Path] = e.Entry
					}
				}
				var entries []proto.ManifestEntry
				for _, e := range wanted {
					entries = append(entries, e)
				}
				slices.SortFunc(entries, func(a, b proto.ManifestEntry) int { return strings.Compare(a.Path, b.Path) })
				fresh, fullErr := New(t.Context(), proto.Manifest{Entries: entries})
				next, err := s.Update(t.Context(), edits)
				if (err == nil) != (fullErr == nil) {
					t.Fatalf("update=%v full=%v", err, fullErr)
				}
				if err == nil {
					if !slices.Equal(export(t, next).Entries, export(t, fresh).Entries) || next.Bytes() != fresh.Bytes() || next.Len() != fresh.Len() {
						t.Fatal("different result")
					}
				}
				if !slices.Equal(export(t, s).Entries, base) {
					t.Fatal("mutated base")
				}
			})
		}
	}
}

func BenchmarkHierarchyUpdate(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		for _, shape := range []string{"repeated", "dispersed", "mixed"} {
			b.Run(fmt.Sprintf("%d/%s", count, shape), func(b *testing.B) {
				entries := make([]proto.ManifestEntry, count)
				for i := range entries {
					entries[i] = file(fmt.Sprintf("root/d%04d/nested/file%05d", i/100, i), 1)
				}
				s, err := New(b.Context(), proto.Manifest{Entries: entries})
				if err != nil {
					b.Fatal(err)
				}
				if err := s.PrepareUpdates(b.Context()); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					var edits []Edit
					for i := 0; i < 100; i++ {
						index := i
						if shape != "repeated" {
							index = (n*101 + i*(count/100)) % count
						}
						e := entries[index]
						e.Size = int64(n + 2)
						edits = append(edits, Edit{Entry: e})
					}
					if shape == "mixed" {
						name := "extra"
						if n%2 == 0 {
							edits = append(edits, Edit{Entry: file(name, 1)})
						} else {
							edits = append(edits, Edit{Entry: file(name, 0), Delete: true})
						}
					}
					s, err = s.Update(b.Context(), edits)
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// Reuse an immutable base so construction is outside the timed region. These
// boundaries catch allocation regressions hidden by replacement-only batches.
func BenchmarkHierarchyUpdateBoundaries(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		for _, shape := range []string{"single", "delete-only", "delete-heavy"} {
			b.Run(fmt.Sprintf("%d/%s", count, shape), func(b *testing.B) {
				entries := make([]proto.ManifestEntry, count)
				for i := range entries {
					entries[i] = file(fmt.Sprintf("root/d%04d/nested/file%05d", i/100, i), 1)
				}
				s, err := New(b.Context(), proto.Manifest{Entries: entries})
				if err != nil {
					b.Fatal(err)
				}
				if err := s.PrepareUpdates(b.Context()); err != nil {
					b.Fatal(err)
				}
				var edits []Edit
				wantCount, wantBytes := count, int64(count)
				for i := 0; i < 100; i++ {
					e := entries[i]
					if shape == "single" || shape == "delete-heavy" && i == 0 {
						e.Size = 2
						edits = append(edits, Edit{Entry: e})
						wantBytes++
					} else {
						edits = append(edits, Edit{Entry: e, Delete: true})
						wantCount--
						wantBytes--
					}
					if shape == "single" {
						break
					}
				}
				var next *Snapshot
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					next, err = s.Update(b.Context(), edits)
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				if next.Len() != wantCount || next.Bytes() != wantBytes || s.Len() != count || s.Bytes() != int64(count) {
					b.Fatal("boundary update changed contents or base")
				}
			})
		}
	}
}
