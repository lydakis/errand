package changes

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/manifest"
	"github.com/lydakis/errand/internal/proto"
)

// Measure retained metadata with an explicit full export, which exercises the
// shared wire API. Compact delta pushes omit that durable export; command
// benchmarks measure the actual push/watch path separately.
func BenchmarkRetainedTransferMetadata(b *testing.B) {
	for _, count := range []int{1000, 10000, 100000} {
		for _, edits := range []int{1, 100} {
			b.Run(fmt.Sprintf("%d/edit%d", count, edits), func(b *testing.B) {
				m := proto.Manifest{Entries: make([]proto.ManifestEntry, count)}
				for i := range m.Entries {
					m.Entries[i] = proto.ManifestEntry{Path: fmt.Sprintf("file-%06d", i), Type: proto.EntryFile, Mode: 0644, Size: 1024, SHA256: fmt.Sprintf("%064x", i)}
				}
				before, err := manifest.New(context.Background(), m)
				if err != nil {
					b.Fatal(err)
				}
				if err := before.PrepareUpdates(context.Background()); err != nil {
					b.Fatal(err)
				}
				if _, err := before.RootHash(context.Background()); err != nil {
					b.Fatal(err)
				}
				var update, compare, accept, wire time.Duration
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					start := time.Now()
					batch := make([]manifest.Edit, edits)
					for j := range batch {
						e := m.Entries[j*(count/edits)]
						e.SHA256 = fmt.Sprintf("%064x", count+i)
						batch[j] = manifest.Edit{Entry: e}
					}
					next, err := before.Update(context.Background(), batch)
					if err != nil {
						b.Fatal(err)
					}
					update += time.Since(start)
					start = time.Now()
					plan, err := PrepareSnapshotDelta(context.Background(), before, next, 1<<30)
					if err != nil {
						b.Fatal(err)
					}
					delta := plan.Bundle()
					compare += time.Since(start)
					start = time.Now()
					accepted, err := plan.Accepted(context.Background(), delta.Paths)
					if err != nil {
						b.Fatal(err)
					}
					accept += time.Since(start)
					start = time.Now()
					if _, err := next.RootHash(context.Background()); err != nil {
						b.Fatal(err)
					}
					exported, err := next.Manifest(context.Background())
					if err != nil {
						b.Fatal(err)
					}
					if _, err := json.Marshal(exported); err != nil {
						b.Fatal(err)
					}
					// Next cycle consumes the accepted checkpoint identity.
					if _, err := accepted.RootHash(context.Background()); err != nil {
						b.Fatal(err)
					}
					wire += time.Since(start)
					before = accepted
				}
				b.ReportMetric(float64(update.Nanoseconds())/float64(b.N), "update-ns/op")
				b.ReportMetric(float64(compare.Nanoseconds())/float64(b.N), "compare-ns/op")
				b.ReportMetric(float64(accept.Nanoseconds())/float64(b.N), "accept-ns/op")
				b.ReportMetric(float64(wire.Nanoseconds())/float64(b.N), "wire-ns/op")
			})
		}
	}
}
