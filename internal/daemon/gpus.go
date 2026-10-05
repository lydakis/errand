package daemon

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lydakis/errand/internal/nowindow"
	"github.com/lydakis/errand/internal/proto"
)

// GPU inventory changes only with hardware or driver changes, while info is
// probed on every --where selection, so the driver is asked at most once a
// minute, in the background: a slow or hung driver must not hold up info
// past the client's placement deadline. Only the very first measurement is
// waited for, briefly. A failed or absent driver reports no GPUs.
const (
	gpuFactsTTL     = time.Minute
	gpuProbeTimeout = 10 * time.Second
	gpuFirstWait    = time.Second
)

type gpuCache struct {
	mu      sync.Mutex
	at      time.Time // when gpus was measured; zero until the first probe ends
	gpus    []proto.GPU
	running chan struct{} // closed when the probe in flight ends
	waited  bool          // the first measurement was already waited for
	probe   func(context.Context) []proto.GPU
}

func (c *gpuCache) measure() []proto.GPU {
	c.mu.Lock()
	if c.at.IsZero() || time.Since(c.at) > gpuFactsTTL {
		c.refreshLocked()
	}
	first, done := c.at.IsZero() && !c.waited, c.running
	c.waited = true
	c.mu.Unlock()
	if first {
		select {
		case <-done:
		case <-time.After(gpuFirstWait):
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]proto.GPU(nil), c.gpus...)
}

func (c *gpuCache) refreshLocked() {
	if c.running != nil {
		return
	}
	done := make(chan struct{})
	c.running = done
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), gpuProbeTimeout)
		gpus := c.probe(ctx)
		cancel()
		c.mu.Lock()
		c.gpus, c.at, c.running = gpus, time.Now(), nil
		c.mu.Unlock()
	}()
}

func probeNVIDIA(ctx context.Context) []proto.GPU {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, path, "--query-gpu=name,memory.total", "--format=csv,noheader,nounits")
	cmd.WaitDelay = 100 * time.Millisecond
	nowindow.Hide(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseNVIDIA(out)
}

// parseNVIDIA reads "NVIDIA H100 80GB HBM3, 81559" lines. Memory is "[N/A]"
// on unified-memory parts; the GPU still counts.
func parseNVIDIA(out []byte) []proto.GPU {
	var gpus []proto.GPU
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		name, mem, _ := strings.Cut(sc.Text(), ",")
		name = strings.TrimSpace(name)
		if name == "" || len(name) > 128 || len(gpus) >= 1024 {
			continue
		}
		mib, _ := strconv.Atoi(strings.TrimSpace(mem))
		gpus = append(gpus, proto.GPU{Name: name, MemoryMiB: max(mib, 0)})
	}
	return gpus
}
