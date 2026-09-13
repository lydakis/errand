package changes

import (
	"fmt"
	"slices"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

// The public API and fixture also compile at the pre-integration revision.
func BenchmarkPreparedMetadata(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		for _, nested := range []bool{false, true} {
			b.Run(fmt.Sprintf("%d/nested=%t", count, nested), func(b *testing.B) {
				base := proto.Manifest{Entries: make([]proto.ManifestEntry, count)}
				for i := range base.Entries {
					name := fmt.Sprintf("file-%05d", i)
					if nested {
						name = fmt.Sprintf("packages/pkg-%03d/src/%s", i/100, name)
					}
					base.Entries[i] = proto.ManifestEntry{Path: name, Type: proto.EntryFile, Mode: 0644, Size: 1024, SHA256: fmt.Sprintf("%064x", i)}
				}
				current := proto.Manifest{Entries: slices.Clone(base.Entries)}
				current.Entries[0].SHA256 = fmt.Sprintf("%064x", count)
				prepared, err := PrepareTransferSource(b.Context(), base, current, 1<<30)
				if err != nil {
					b.Fatal(err)
				}
				delta, root := prepared.Delta(), current.RootHash()
				b.Run("prepare", func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						if _, err := PrepareTransferSource(b.Context(), base, current, 1<<30); err != nil {
							b.Fatal(err)
						}
					}
				})
				b.Run("expand", func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						if _, err := ExpandTransferSource(b.Context(), base, delta, root, 1<<30); err != nil {
							b.Fatal(err)
						}
					}
				})
			})
		}
	}
}
