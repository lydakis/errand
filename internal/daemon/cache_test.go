package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

func testCache(t *testing.T, maxBytes int64, ttl time.Duration) *blobCache {
	t.Helper()
	c, err := newBlobCache(filepath.Join(t.TempDir(), "blobs"), maxBytes, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func insertContent(t *testing.T, c *blobCache, content string) (sha string, size int64) {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	sha = hex.EncodeToString(sum[:])
	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Insert(context.Background(), src, sha, int64(len(content))); err != nil {
		t.Fatal(err)
	}
	return sha, int64(len(content))
}

func TestCacheInsertMaterializeRoundTrip(t *testing.T) {
	c := testCache(t, 1<<20, time.Hour)
	sha, size := insertContent(t, c, "hello cache")
	dest := filepath.Join(t.TempDir(), "out.txt")
	hit, err := c.Materialize(context.Background(), dest, proto.ManifestEntry{
		Path: "out.txt", Type: proto.EntryFile, Mode: 0o640, Size: size, SHA256: sha,
	})
	if err != nil || !hit {
		t.Fatalf("materialize hit=%v err=%v", hit, err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "hello cache" {
		t.Fatalf("materialized content %q, %v", got, err)
	}
	fi, _ := os.Stat(dest)
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("materialized mode %04o, want 0640", fi.Mode().Perm())
	}
	if missing := c.Missing([]proto.BlobRef{{SHA256: sha, Size: size}}); len(missing) != 0 {
		t.Fatalf("cached blob reported missing: %v", missing)
	}
}

func TestCacheInsertRejectsContentThatDoesNotMatchHash(t *testing.T) {
	c := testCache(t, 1<<20, time.Hour)
	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte("right"))
	sha := hex.EncodeToString(want[:])
	if err := c.Insert(context.Background(), src, sha, int64(len("wrong"))); err == nil {
		t.Fatal("cache accepted content that did not match its address")
	}
	if _, err := os.Lstat(c.path(sha)); !os.IsNotExist(err) {
		t.Fatalf("mismatched content was published: %v", err)
	}
}

func TestCacheCorruptBlobDegradesToMiss(t *testing.T) {
	c := testCache(t, 1<<20, time.Hour)
	sha, size := insertContent(t, c, "pristine content")
	// Corrupt the blob in place, keeping its size.
	if err := os.WriteFile(c.path(sha), []byte("corrupted conten"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "f")
	hit, err := c.Materialize(context.Background(), dest, proto.ManifestEntry{
		Path: "f", Type: proto.EntryFile, Mode: 0o644, Size: size, SHA256: sha,
	})
	if err != nil || hit {
		t.Fatalf("corrupt blob materialized: hit=%v err=%v", hit, err)
	}
	if _, err := os.Lstat(c.path(sha)); !os.IsNotExist(err) {
		t.Fatal("corrupt blob was not deleted")
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatal("partial materialization left a file behind")
	}
}

func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := testCache(t, 25, time.Hour)
	oldSha, _ := insertContent(t, c, "0123456789") // 10 bytes
	// Age the first blob so LRU ordering is unambiguous.
	past := time.Now().Add(-time.Minute)
	os.Chtimes(c.path(oldSha), past, past)
	newSha, _ := insertContent(t, c, "abcdefghij") // 10 bytes, total 20 <= 25
	if _, err := os.Lstat(c.path(oldSha)); err != nil {
		t.Fatalf("old blob evicted prematurely: %v", err)
	}
	third, _ := insertContent(t, c, "KLMNOPQRST") // total 30 > 25: evict LRU
	if _, err := os.Lstat(c.path(oldSha)); !os.IsNotExist(err) {
		t.Fatal("LRU blob survived eviction")
	}
	for _, sha := range []string{newSha, third} {
		if _, err := os.Lstat(c.path(sha)); err != nil {
			t.Fatalf("recent blob %s evicted: %v", sha[:8], err)
		}
	}
}

func TestCacheEvictionKeepsHeadroom(t *testing.T) {
	c := testCache(t, 100, time.Hour)
	for i := 0; i < 6; i++ {
		insertContent(t, c, strings.Repeat(string(rune('a'+i)), 20))
	}
	stats, err := c.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Bytes > 90 {
		t.Fatalf("cache retained %d bytes after eviction, want at most 90 bytes of a 100-byte cache", stats.Bytes)
	}
}

func TestCacheGCPrunesExpiredBlobs(t *testing.T) {
	c := testCache(t, 1<<20, time.Hour)
	sha, size := insertContent(t, c, "soon to expire")
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(c.path(sha), old, old)
	result, err := c.GC()
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedBlobs != 1 || result.FreedBytes != size {
		t.Fatalf("gc result = %+v", result)
	}
	stats, err := c.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Blobs != 0 || stats.Bytes != 0 {
		t.Fatalf("stats after gc = %+v", stats)
	}
}

func TestCacheGCRemovesCrashInsertTemp(t *testing.T) {
	c := testCache(t, 1<<20, time.Hour)
	temp := filepath.Join(c.dir, ".insert-orphan")
	if err := os.WriteFile(temp, []byte("partial blob"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := c.GC()
	if err != nil {
		t.Fatal(err)
	}
	if result.FreedBytes != int64(len("partial blob")) {
		t.Fatalf("gc result = %+v", result)
	}
	if _, err := os.Lstat(temp); !os.IsNotExist(err) {
		t.Fatalf("crash temp survived gc: %v", err)
	}
}

func TestCacheGCDryRunReportsWithoutRemovingAnything(t *testing.T) {
	c := testCache(t, 1<<20, time.Hour)
	sha, size := insertContent(t, c, "soon to expire")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(c.path(sha), old, old); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(c.dir, ".insert-orphan")
	if err := os.WriteFile(temp, []byte("partial blob"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := c.GCContext(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || result.RemovedBlobs != 1 || result.FreedBytes != size+int64(len("partial blob")) {
		t.Fatalf("dry-run result = %+v", result)
	}
	for _, path := range []string{c.path(sha), temp} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("dry-run removed %q: %v", path, err)
		}
	}
}

func TestCacheGCDryRunUsesTheRealCapacityPolicy(t *testing.T) {
	c := testCache(t, 50, time.Hour)
	type cached struct {
		path string
		size int64
	}
	var blobs []cached
	for index, content := range []string{
		"aaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbb",
		"cccccccccccccccccccc",
	} {
		sum := sha256.Sum256([]byte(content))
		sha := hex.EncodeToString(sum[:])
		path := c.path(sha)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		used := time.Now().Add(time.Duration(index-3) * time.Minute)
		if err := os.Chtimes(path, used, used); err != nil {
			t.Fatal(err)
		}
		blobs = append(blobs, cached{path: path, size: int64(len(content))})
	}

	preview, err := c.GCContext(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if preview.RemovedBlobs != 1 || preview.FreedBytes != blobs[0].size {
		t.Fatalf("capacity preview = %+v", preview)
	}
	for _, blob := range blobs {
		if _, err := os.Stat(blob.path); err != nil {
			t.Fatalf("capacity preview removed %q: %v", blob.path, err)
		}
	}

	actual, err := c.GC()
	if err != nil {
		t.Fatal(err)
	}
	if actual.RemovedBlobs != preview.RemovedBlobs || actual.FreedBytes != preview.FreedBytes {
		t.Fatalf("actual GC %+v did not match preview %+v", actual, preview)
	}
	if _, err := os.Stat(blobs[0].path); !os.IsNotExist(err) {
		t.Fatalf("actual GC kept oldest blob: %v", err)
	}
}

func TestCacheStatsReportsInvalidBlobEntry(t *testing.T) {
	c := testCache(t, 1<<20, time.Hour)
	path := filepath.Join(c.dir, strings.Repeat("a", 64))
	if err := os.Symlink("missing", path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stats(); err == nil {
		t.Fatal("invalid cache entry was silently ignored")
	}
}

func TestCacheMissingTouchesPresentBlobs(t *testing.T) {
	c := testCache(t, 1<<20, time.Hour)
	sha, size := insertContent(t, c, "touch me")
	old := time.Now().Add(-30 * time.Minute)
	os.Chtimes(c.path(sha), old, old)
	unknown := "ab" + sha[2:] // valid shape, not present
	missing := c.Missing([]proto.BlobRef{{SHA256: sha, Size: size}, {SHA256: unknown, Size: 4}})
	if len(missing) != 1 || missing[0] != unknown {
		t.Fatalf("missing = %v, want only %s", missing, unknown[:8])
	}
	fi, err := os.Lstat(c.path(sha))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().After(old.Add(time.Minute)) {
		t.Fatal("negotiation did not touch the present blob")
	}
}

func TestCacheMissingExpiresIdleBlobBeforeTouchingIt(t *testing.T) {
	c := testCache(t, 1<<20, time.Hour)
	sha, size := insertContent(t, c, "expire me")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(c.path(sha), old, old); err != nil {
		t.Fatal(err)
	}

	missing := c.Missing([]proto.BlobRef{{SHA256: sha, Size: size}})
	if len(missing) != 1 || missing[0] != sha {
		t.Fatalf("expired cache lookup = %v, want %s", missing, sha)
	}
	if _, err := os.Lstat(c.path(sha)); !os.IsNotExist(err) {
		t.Fatalf("expired blob was retained or revived: %v", err)
	}
}

func TestCacheInsertCanBeCanceledWhileWaiting(t *testing.T) {
	c := testCache(t, 1<<20, time.Hour)
	if err := c.acquireInsert(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.releaseInsert()

	src := filepath.Join(t.TempDir(), "src")
	content := []byte("cancel me")
	if err := os.WriteFile(src, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Insert(ctx, src, hex.EncodeToString(sum[:]), int64(len(content))); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled insert error = %v, want context.Canceled", err)
	}
}

func TestCacheMaterializeHonorsCanceledContext(t *testing.T) {
	c := testCache(t, 1<<20, time.Hour)
	sha, size := insertContent(t, c, "do not copy")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dest := filepath.Join(t.TempDir(), "out")
	hit, err := c.Materialize(ctx, dest, proto.ManifestEntry{
		Path: "out", Type: proto.EntryFile, Mode: 0o600, Size: size, SHA256: sha,
	})
	if hit || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled materialize = hit %v, error %v", hit, err)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("canceled materialize left destination: %v", err)
	}
}

func TestCacheCorruptionRemovalCanBeCanceledWhileWaiting(t *testing.T) {
	c := testCache(t, 1<<20, time.Hour)
	sha, _ := insertContent(t, c, "corrupt me")
	c.mu.Lock()
	defer c.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.removeIfCurrent(ctx, sha, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled corruption removal error = %v, want context.Canceled", err)
	}
	if _, err := os.Lstat(c.path(sha)); err != nil {
		t.Fatalf("canceled removal changed the blob: %v", err)
	}
}

func TestCacheCorruptionRemovalPreservesReplacement(t *testing.T) {
	c := testCache(t, 1<<20, time.Hour)
	sha, _ := insertContent(t, c, "replacement content")
	p := c.path(sha)
	original, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(t.TempDir(), "replacement")
	if err := os.WriteFile(replacement, []byte("replacement content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, p); err != nil {
		t.Fatal(err)
	}

	if err := c.removeIfCurrent(context.Background(), sha, original); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p); err != nil {
		t.Fatalf("corruption cleanup removed a replacement blob: %v", err)
	}
}

func TestCacheInsertRollsBackWhenEvictionFails(t *testing.T) {
	c := testCache(t, 15, time.Hour)
	insertContent(t, c, "0123456789")
	if err := os.WriteFile(filepath.Join(c.dir, "unexpected"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	content := []byte("abcdefghij")
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])
	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Insert(context.Background(), src, sha, int64(len(content))); err == nil {
		t.Fatal("insert succeeded despite eviction scan failure")
	}
	if _, err := os.Lstat(c.path(sha)); !os.IsNotExist(err) {
		t.Fatalf("failed insert left its published blob behind: %v", err)
	}
	if c.bytes != int64(len("0123456789")) {
		t.Fatalf("cache accounting after rollback = %d, want 10", c.bytes)
	}
}
