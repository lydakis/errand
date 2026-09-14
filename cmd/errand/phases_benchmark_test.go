package main

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/trace"
	"testing"
	"time"
)

// Phase logs include untimed fixture and cleanup work. Regions also make these
// boundaries visible in an optional Go trace without changing CLI receipts.
func benchmarkPhase(b *testing.B, name string) func() {
	region := trace.StartRegion(context.Background(), name)
	emit := func(event string, elapsed time.Duration) {
		row, _ := json.Marshal(map[string]any{
			"benchmark": b.Name(), "iterations": b.N, "phase": name,
			"event": event, "seconds": elapsed.Seconds(),
		})
		fmt.Printf("BENCH_PHASE %s\n", row)
	}
	emit("start", 0)
	started := time.Now()
	return func() {
		elapsed := time.Since(started)
		region.End()
		emit("end", elapsed)
	}
}

// Register the end before any TempDir callbacks, and the start after them.
// Deferred daemon/server shutdown happens before testing runs these callbacks.
func benchmarkCleanupPhase(b *testing.B) func() {
	var finish func()
	b.Cleanup(func() {
		if finish != nil {
			finish()
		}
	})
	return func() {
		b.Cleanup(func() { finish = benchmarkPhase(b, "temp-cleanup") })
	}
}
