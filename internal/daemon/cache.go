package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

const insertTempPrefix = ".insert-"

type contextMutex chan struct{}

func newContextMutex() contextMutex {
	m := make(contextMutex, 1)
	m <- struct{}{}
	return m
}

func (m contextMutex) Lock() { <-m }

func (m contextMutex) LockContext(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m:
	}
	if err := ctx.Err(); err != nil {
		m.Unlock()
		return err
	}
	return nil
}

func (m contextMutex) Unlock() { m <- struct{}{} }

// blobCache stores hash-verified snapshot content. File mtime records last use.
type blobCache struct {
	insertMu contextMutex
	mu       contextMutex
	dir      string
	maxBytes int64
	ttl      time.Duration
	bytes    int64
}

func newBlobCache(dir string, maxBytes int64, ttl time.Duration) (*blobCache, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	c := &blobCache{
		insertMu: newContextMutex(), mu: newContextMutex(),
		dir: dir, maxBytes: maxBytes, ttl: ttl,
	}
	if _, err := c.GC(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *blobCache) path(sha string) string {
	return filepath.Join(c.dir, sha[:2], sha)
}

func validBlobHash(sha string) bool {
	if len(sha) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(sha)
	return err == nil
}

func (c *blobCache) acquireInsert(ctx context.Context) error {
	return c.insertMu.LockContext(ctx)
}

func (c *blobCache) releaseInsert() {
	c.insertMu.Unlock()
}

func (c *blobCache) expired(fi fs.FileInfo, now time.Time) bool {
	return fi.ModTime().Before(now.Add(-c.ttl))
}

// Missing touches hits so negotiation defers their eviction.
func (c *blobCache) Missing(blobs []proto.BlobRef) []string {
	missing, _ := c.MissingContext(context.Background(), blobs)
	return missing
}

func (c *blobCache) MissingContext(ctx context.Context, blobs []proto.BlobRef) ([]string, error) {
	if err := c.mu.LockContext(ctx); err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	now := time.Now()
	missing := make([]string, 0)
	seen := make(map[string]bool, len(blobs))
	for _, b := range blobs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if seen[b.SHA256] {
			continue
		}
		seen[b.SHA256] = true
		if !validBlobHash(b.SHA256) {
			missing = append(missing, b.SHA256)
			continue
		}
		p := c.path(b.SHA256)
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() && fi.Size() == b.Size {
			if c.expired(fi, now) {
				if err := os.Remove(p); err == nil {
					c.bytes -= fi.Size()
				}
				missing = append(missing, b.SHA256)
				continue
			}
			os.Chtimes(p, now, now)
			continue
		}
		missing = append(missing, b.SHA256)
	}
	return missing, nil
}

// Materialize verifies content while copying; corruption becomes a miss.
func (c *blobCache) Materialize(ctx context.Context, dest string, e proto.ManifestEntry) (bool, error) {
	if !validBlobHash(e.SHA256) {
		return false, nil
	}
	p := c.path(e.SHA256)
	if err := c.mu.LockContext(ctx); err != nil {
		return false, err
	}
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != e.Size {
		c.mu.Unlock()
		return false, nil
	}
	if c.expired(fi, time.Now()) {
		if err := os.Remove(p); err == nil {
			c.bytes -= fi.Size()
		}
		c.mu.Unlock()
		return false, nil
	}
	src, err := os.Open(p)
	c.mu.Unlock()
	if err != nil {
		return false, nil // miss
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, err
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(e.Mode))
	if err != nil {
		return false, err
	}
	h := sha256.New()
	n, copyErr := io.Copy(
		io.MultiWriter(out, h),
		io.LimitReader(&contextReader{ctx: ctx, r: src}, e.Size+1),
	)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(dest)
		if copyErr == nil {
			copyErr = closeErr
		}
		return false, copyErr
	}
	if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		os.Remove(dest)
		if err := c.removeIfCurrent(ctx, e.SHA256, fi); err != nil {
			return false, err
		}
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		os.Remove(dest)
		return false, err
	}
	if err := os.Chmod(dest, os.FileMode(e.Mode)); err != nil {
		os.Remove(dest)
		return false, err
	}
	now := time.Now()
	if err := c.mu.LockContext(ctx); err != nil {
		os.Remove(dest)
		return false, err
	}
	os.Chtimes(p, now, now)
	c.mu.Unlock()
	return true, nil
}

func (c *blobCache) Insert(ctx context.Context, src, sha string, size int64) error {
	if !validBlobHash(sha) || size < 0 || size > c.maxBytes {
		return nil // never cache what could not fit or be addressed
	}
	if err := c.acquireInsert(ctx); err != nil {
		return err
	}
	defer c.releaseInsert()

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(c.dir, insertTempPrefix+"*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	h := sha256.New()
	n, err := io.Copy(
		io.MultiWriter(tmp, h),
		io.LimitReader(&contextReader{ctx: ctx, r: in}, size+1),
	)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != sha {
		return fmt.Errorf("cache: %s changed or did not match its content hash while inserting", sha)
	}

	if err := c.mu.LockContext(ctx); err != nil {
		return err
	}
	defer c.mu.Unlock()
	p := c.path(sha)
	var replacedBytes int64
	if fi, err := os.Lstat(p); err == nil {
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("cache: blob %q is not a regular file", p)
		}
		replacedBytes = fi.Size()
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return err
	}
	c.bytes += size - replacedBytes
	if err := c.enforceSizeLocked(ctx); err != nil {
		fi, statErr := os.Lstat(p)
		if statErr == nil {
			if removeErr := os.Remove(p); removeErr != nil {
				return fmt.Errorf("%w; rolling back cache publication: %v", err, removeErr)
			}
			c.bytes -= fi.Size()
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("%w; inspecting cache publication during rollback: %v", err, statErr)
		}
		return err
	}
	return nil
}

func (c *blobCache) remove(sha string) {
	_ = c.removeIfCurrent(context.Background(), sha, nil)
}

func (c *blobCache) removeIfCurrent(ctx context.Context, sha string, expected fs.FileInfo) error {
	if !validBlobHash(sha) {
		return fmt.Errorf("cache: invalid blob hash %q", sha)
	}
	if err := c.mu.LockContext(ctx); err != nil {
		return err
	}
	defer c.mu.Unlock()
	p := c.path(sha)
	fi, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if expected != nil && !os.SameFile(expected, fi) {
		return nil
	}
	if err := os.Remove(p); err != nil {
		return err
	}
	c.bytes -= fi.Size()
	return nil
}

func (c *blobCache) Stats() (proto.CacheStats, error) {
	return c.StatsContext(context.Background())
}

func (c *blobCache) StatsContext(ctx context.Context) (proto.CacheStats, error) {
	if err := c.mu.LockContext(ctx); err != nil {
		return proto.CacheStats{}, err
	}
	defer c.mu.Unlock()
	blobs, bytes, err := c.walkLocked(ctx, nil)
	if err != nil {
		return proto.CacheStats{}, err
	}
	c.bytes = bytes
	return proto.CacheStats{
		Blobs: blobs, Bytes: bytes,
		MaxBytes: c.maxBytes, TTLHours: int(c.ttl.Hours()),
	}, nil
}

func (c *blobCache) GC() (proto.CacheGCResult, error) {
	return c.GCContext(context.Background())
}

func (c *blobCache) GCContext(ctx context.Context) (proto.CacheGCResult, error) {
	if err := c.acquireInsert(ctx); err != nil {
		return proto.CacheGCResult{}, err
	}
	defer c.releaseInsert()
	if err := c.mu.LockContext(ctx); err != nil {
		return proto.CacheGCResult{}, err
	}
	defer c.mu.Unlock()
	var result proto.CacheGCResult
	tempBytes, err := c.cleanupTempsLocked(ctx)
	if err != nil {
		return result, err
	}
	result.FreedBytes = tempBytes
	cutoff := time.Now().Add(-c.ttl)
	_, bytes, err := c.walkLocked(ctx, func(path string, fi fs.FileInfo) error {
		if fi.ModTime().Before(cutoff) {
			if err := os.Remove(path); err != nil {
				return err
			}
			c.bytes -= fi.Size()
			result.RemovedBlobs++
			result.FreedBytes += fi.Size()
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	c.bytes = bytes
	evicted, freed, err := c.evictOverLocked(ctx)
	result.RemovedBlobs += evicted
	result.FreedBytes += freed
	return result, err
}

func (c *blobCache) enforceSizeLocked(ctx context.Context) error {
	_, _, err := c.evictOverLocked(ctx)
	return err
}

func (c *blobCache) evictOverLocked(ctx context.Context) (int, int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	if c.bytes <= c.maxBytes {
		return 0, 0, nil
	}
	type blob struct {
		path string
		size int64
		used time.Time
	}
	var blobs []blob
	_, total, err := c.walkLocked(ctx, func(path string, fi fs.FileInfo) error {
		blobs = append(blobs, blob{path: path, size: fi.Size(), used: fi.ModTime()})
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	c.bytes = total
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].used.Before(blobs[j].used) })
	target := c.maxBytes
	if target >= 10 {
		target -= target / 10
	}
	var removed int
	var freed int64
	for _, b := range blobs {
		if err := ctx.Err(); err != nil {
			return removed, freed, err
		}
		if c.bytes <= target {
			break
		}
		if err := os.Remove(b.path); err != nil {
			return removed, freed, err
		}
		c.bytes -= b.size
		removed++
		freed += b.size
	}
	return removed, freed, nil
}

func (c *blobCache) cleanupTempsLocked(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return 0, err
	}
	var freed int64
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return freed, err
		}
		if !strings.HasPrefix(entry.Name(), insertTempPrefix) {
			continue
		}
		path := filepath.Join(c.dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return freed, err
		}
		if !info.Mode().IsRegular() {
			return freed, fmt.Errorf("cache: insert temporary %q is not a regular file", path)
		}
		if err := os.Remove(path); err != nil {
			return freed, err
		}
		freed += info.Size()
	}
	return freed, nil
}

func (c *blobCache) walkLocked(ctx context.Context, fn func(path string, fi fs.FileInfo) error) (int, int64, error) {
	var count int
	var bytes int64
	err := filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Dir(path) == c.dir && strings.HasPrefix(d.Name(), insertTempPrefix) {
			return nil
		}
		if !validBlobHash(d.Name()) {
			return fmt.Errorf("cache: unexpected entry %q", path)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("cache: blob %q is not a regular file", path)
		}
		if fn != nil {
			if err := fn(path, fi); err != nil {
				return err
			}
		}
		current, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !current.Mode().IsRegular() {
			return fmt.Errorf("cache: blob %q changed type during scan", path)
		}
		count++
		bytes += current.Size()
		return nil
	})
	return count, bytes, err
}
