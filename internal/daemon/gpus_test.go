package daemon

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

func TestParseNVIDIA(t *testing.T) {
	out := []byte("NVIDIA H100 80GB HBM3, 81559\nNVIDIA GB10, [N/A]\n\n, 12\n")
	want := []proto.GPU{{Name: "NVIDIA H100 80GB HBM3", MemoryMiB: 81559}, {Name: "NVIDIA GB10"}}
	if got := parseNVIDIA(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestGPUFactsDoNotWaitForSlowDriver(t *testing.T) {
	release := make(chan struct{})
	calls := make(chan struct{}, 4)
	h100 := []proto.GPU{{Name: "NVIDIA H100", MemoryMiB: 81559}}
	c := gpuCache{probe: func(ctx context.Context) []proto.GPU {
		calls <- struct{}{}
		select {
		case <-release:
			return h100
		case <-ctx.Done():
			return nil
		}
	}}
	start := time.Now()
	if got := c.measure(); got != nil {
		t.Fatalf("first measure while the driver hangs = %+v", got)
	}
	if waited := time.Since(start); waited < gpuFirstWait || waited > gpuFirstWait+time.Second {
		t.Fatalf("first measure waited %s, want about %s", waited, gpuFirstWait)
	}
	// Later calls never wait, and do not start a second probe.
	start = time.Now()
	c.measure()
	if waited := time.Since(start); waited > 100*time.Millisecond {
		t.Fatalf("second measure waited %s", waited)
	}
	close(release)
	<-calls
	deadline := time.Now().Add(5 * time.Second)
	for !reflect.DeepEqual(c.measure(), h100) {
		if time.Now().After(deadline) {
			t.Fatal("finished probe never reported")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(calls) != 0 {
		t.Fatalf("%d extra probes ran", len(calls))
	}
}
