package daemon

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/proto"
)

// A job's change base reads the submitted bodies of its changed files from the
// snapshot cache. The job pins those blobs from staging until it settles, so
// eviction, expiry and GC cannot remove them meanwhile. Pins live in memory:
// nothing reads a job base after a daemon restart. Pinned blobs can hold the
// cache above its byte budget until their jobs settle.

// pinTouchInterval bounds how often a pin refreshes a blob's last use. Last
// use only orders eviction, and a job on a persistent workspace pins every body.
const pinTouchInterval = time.Hour

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

// release drops the job's pins. It is idempotent.
func (p *cachePins) release() {
	if p == nil {
		return
	}
	c := p.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	for sha := range p.shas {
		c.unpinLocked(sha)
	}
	p.shas = nil
	p.released = true
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

// pinPresent pins the cached bodies of m's files and returns one entry for
// each body the cache does not hold. A nil cache holds nothing.
func (c *blobCache) pinPresent(ctx context.Context, m proto.Manifest, p *cachePins) ([]proto.ManifestEntry, error) {
	var missing []proto.ManifestEntry
	if c == nil {
		seen := make(map[string]bool)
		for _, e := range m.Entries {
			if e.Type == proto.EntryFile && !seen[e.SHA256] {
				seen[e.SHA256] = true
				missing = append(missing, e)
			}
		}
		return missing, nil
	}
	if err := c.mu.LockContext(ctx); err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	now := time.Now()
	seen := make(map[string]bool)
	for _, e := range m.Entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.Type != proto.EntryFile || seen[e.SHA256] {
			continue
		}
		seen[e.SHA256] = true
		if !validBlobHash(e.SHA256) {
			missing = append(missing, e)
			continue
		}
		path := c.path(e.SHA256)
		fi, err := os.Lstat(path)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() != e.Size || (c.expired(fi, now) && !c.pinnedLocked(e.SHA256)) {
			missing = append(missing, e)
			continue
		}
		c.pinLocked(p, e.SHA256)
		if now.Sub(fi.ModTime()) > pinTouchInterval {
			os.Chtimes(path, now, now)
		}
	}
	return missing, nil
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
