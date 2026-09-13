package snapshotindex

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

var benchIndex Index
var benchEdits []Edit
var benchDigest [32]byte
var benchData []byte

func BenchmarkIndex(b *testing.B) {
	for _, count := range []int{1000, 10000, 100000} {
		entries := fixture(count)
		one := entries[count/2]
		one.SHA256 = fmt.Sprintf("%064x", count+1)
		many := make([]Edit, 100)
		for i := range many {
			entry := entries[i*(count/100)]
			entry.Mode = 0755
			many[i] = Edit{Entry: entry}
		}
		added := proto.ManifestEntry{Path: "new-file", Type: proto.EntryFile, Mode: 0644, Size: 1024, SHA256: one.SHA256}
		// A deliberate difficult deletion: the tree root changes. Every
		// representation receives the same edit, including the old fallback.
		rootEntry := NewTree(entries).(*tree).root.entry
		cases := []struct {
			name  string
			edits []Edit
		}{
			{"edit1", []Edit{{Entry: one}}},
			{"edit100", many},
			{"add1", []Edit{{Entry: added}}},
			{"delete_root", []Edit{{Entry: rootEntry, Delete: true}}},
		}
		for _, factory := range factories {
			b.Run(fmt.Sprintf("%d/%s", count, factory.name), func(b *testing.B) {
				base := factory.build(entries)
				b.Run("build", func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						benchIndex = factory.build(entries)
						benchDigest = benchIndex.Digest()
					}
				})
				for _, c := range cases {
					b.Run(c.name, func(b *testing.B) {
						b.ReportAllocs()
						for b.Loop() {
							benchIndex = base.Update(c.edits)
							benchEdits = base.Diff(benchIndex)
							benchDigest = benchIndex.Digest()
						}
					})
				}
				b.Run("edit1_export", func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						next := base.Update(cases[0].edits)
						benchEdits = base.Diff(next)
						data, err := json.Marshal(next.Manifest())
						if err != nil {
							b.Fatal(err)
						}
						benchData = data
						// The flat digest already is the wire root. Other designs
						// hash the encoded manifest to produce today's root.
						if _, ok := next.(*flat); ok {
							benchDigest = next.Digest()
						} else {
							benchDigest = sha256.Sum256(data)
						}
					}
				})
				b.Run("rebuild_diff", func(b *testing.B) {
					current := oracle(entries, cases[0].edits)
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						next := factory.build(current)
						benchEdits = base.Diff(next)
						benchIndex = next
					}
				})
				b.Run("equal_diff", func(b *testing.B) {
					next := factory.build(entries)
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						benchEdits = base.Diff(next)
					}
				})
				for _, disjoint := range []bool{false, true} {
					name := "diff_delete100"
					if disjoint {
						name = "diff_disjoint"
					}
					b.Run(name, func(b *testing.B) {
						var current []proto.ManifestEntry
						for i, entry := range entries {
							if disjoint {
								entry.Path = "renamed-" + entry.Path
							} else if i%(count/100) == 0 {
								continue
							}
							current = append(current, entry)
						}
						next := factory.build(current)
						b.ReportAllocs()
						b.ResetTimer()
						for b.Loop() {
							benchEdits = base.Diff(next)
						}
					})
				}
			})
		}
	}
}
