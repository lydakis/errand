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
// minute. A failed or absent driver reports no GPUs.
const gpuFactsTTL = time.Minute

type gpuCache struct {
	mu    sync.Mutex
	at    time.Time
	gpus  []proto.GPU
	probe func(context.Context) []proto.GPU
}

func (c *gpuCache) measure() []proto.GPU {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || time.Since(c.at) > gpuFactsTTL {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		c.gpus = c.probe(ctx)
		cancel()
		c.at = time.Now()
	}
	return append([]proto.GPU(nil), c.gpus...)
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
