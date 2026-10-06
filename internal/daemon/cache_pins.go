package daemon

import (
	"context"
	"fmt"
	"log"
	"sync"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/proto"
)

// An ephemeral job's change base reads the submitted bodies of its changed
// files from the snapshot cache. The job pins those blobs from staging until it
// settles, so eviction, expiry and GC cannot remove them meanwhile. Pins live in
// memory: nothing reads a job base after a daemon restart. Pinned blobs can hold
// the cache above its byte budget until their jobs settle.

// cachePins is one job's hold on cached blobs.
type cachePins struct {
	cache    *blobCache
	mu       sync.Mutex
	shas     map[string]bool
	released bool
}

func (c *blobCache) newPins() *cachePins {
	if c == nil {
		return nil
	}
	return &cachePins{cache: c, shas: make(map[string]bool)}
}

// shared names the pinned blobs a job's change base reads from the cache.
func (p *cachePins) shared() changeops.SharedBlobs {
	if p == nil {
		return nil
	}
	return func(sha string) (string, bool) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if !p.shas[sha] {
			return "", false
		}
		return p.cache.path(sha), true
	}
}

// release drops the job's pins and evicts down to the cache's budget, which
// the pinned blobs may have held it above. It is idempotent.
func (p *cachePins) release() {
	if p == nil {
		return
	}
	c := p.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	p.mu.Lock()
	held := len(p.shas) != 0
	for sha := range p.shas {
		c.unpinLocked(sha)
	}
	p.shas = nil
	p.released = true
	p.mu.Unlock()
	if held {
		if err := c.enforceSizeLocked(context.Background()); err != nil {
			log.Printf("snapshot cache eviction after a job settled: %v", err)
		}
	}
}

// pinLocked adds sha to p. The caller holds c.mu and has seen the blob.
func (c *blobCache) pinLocked(p *cachePins, sha string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released || p.shas[sha] {
		return
	}
	p.shas[sha] = true
	c.pins[sha]++
}

// dropPinLocked removes sha from p, as when its blob proved corrupt.
func (c *blobCache) dropPinLocked(p *cachePins, sha string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shas[sha] {
		delete(p.shas, sha)
		c.unpinLocked(sha)
	}
}

func (c *blobCache) unpinLocked(sha string) {
	if c.pins[sha]--; c.pins[sha] <= 0 {
		delete(c.pins, sha)
	}
}

func (c *blobCache) pinnedLocked(sha string) bool {
	return c.pins[sha] > 0
}

// pinBase gives the job its hold on the cache for its change base.
func (j *Job) pinBase(d *Daemon) *cachePins {
	pins := d.cache.newPins()
	j.mu.Lock()
	j.basePins = pins
	j.mu.Unlock()
	return pins
}

// releaseBase lets the cache evict the job's change base again.
func (j *Job) releaseBase() {
	j.mu.Lock()
	pins := j.basePins
	j.mu.Unlock()
	pins.release()
}

// baseCaptureDetail counts the distinct bodies a job copied into its private
// store rather than reading them from the cache.
func baseCaptureDetail(m proto.Manifest, shared changeops.SharedBlobs) string {
	seen := make(map[string]bool)
	copied := 0
	for _, e := range m.Entries {
		if e.Type != proto.EntryFile || seen[e.SHA256] {
			continue
		}
		seen[e.SHA256] = true
		if shared == nil {
			copied++
		} else if _, ok := shared(e.SHA256); !ok {
			copied++
		}
	}
	return fmt.Sprintf("root=%s copied=%d/%d", m.RootHash(), copied, len(seen))
}
