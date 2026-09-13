package manifest

import (
	"context"
	"fmt"
	"github.com/lydakis/errand/internal/proto"
	"runtime"
	"testing"
)

// GC is part of this diagnostic, so ns/op is not update latency. Run separately
// with -benchtime=1x. Measure preparation before an edit and its successors.
func BenchmarkSnapshotRetention(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		for _, mode := range []string{"prepared", "updated", "one-survivor"} {
			b.Run(fmt.Sprintf("%d/%s", count, mode), func(b *testing.B) {
				var total int64
				for i := 0; i < b.N; i++ {
					runtime.GC()
					var before, after runtime.MemStats
					runtime.ReadMemStats(&before)
					s := retainedFixture(b, count, mode)
					runtime.GC()
					runtime.ReadMemStats(&after)
					total += int64(after.HeapAlloc) - int64(before.HeapAlloc)
					runtime.KeepAlive(s)
				}
				b.ReportMetric(float64(total)/float64(b.N), "retained-B")
			})
		}
	}
}
func retainedFixture(b *testing.B, count int, mode string) *Snapshot {
	b.Helper()
	entries := make([]proto.ManifestEntry, count)
	for i := range entries {
		entries[i] = file(fmt.Sprintf("file-%05d", i), 1)
	}
	s, err := New(b.Context(), proto.Manifest{Entries: entries})
	if err != nil {
		b.Fatal(err)
	}
	// The preserved hybrid baseline prebuilds its index during preparation.
	if indexed, ok := any(s).(interface{ PrepareUpdates(context.Context) error }); ok {
		if err := indexed.PrepareUpdates(b.Context()); err != nil {
			b.Fatal(err)
		}
	}
	if mode == "prepared" {
		return s
	}
	edits := []Edit{{Entry: file(entries[0].Path, 2)}}
	if mode == "one-survivor" {
		edits = nil
		for _, e := range entries[1:] {
			edits = append(edits, Edit{Entry: e, Delete: true})
		}
	}
	s, err = s.Update(b.Context(), edits)
	if err != nil {
		b.Fatal(err)
	}
	if mode == "one-survivor" && s.Len() != 1 {
		b.Fatal("incorrect survivor count")
	}
	return s
}
