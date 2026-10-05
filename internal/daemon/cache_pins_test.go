package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func blobEntry(content string) proto.ManifestEntry {
	sum := sha256.Sum256([]byte(content))
	return proto.ManifestEntry{Path: "f", Type: proto.EntryFile, Mode: 0o600, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}
}

func insertPinned(t *testing.T, c *blobCache, content string, pins *cachePins) proto.ManifestEntry {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	e := blobEntry(content)
	if err := c.Insert(context.Background(), src, e.SHA256, e.Size, pins); err != nil {
		t.Fatal(err)
	}
	return e
}

func blobPresent(c *blobCache, sha string) bool {
	_, err := os.Lstat(c.path(sha))
	return err == nil
}

func TestPinnedBlobsSurviveEvictionExpiryAndGC(t *testing.T) {
	c := testCache(t, 12, time.Hour)
	pins := c.newPins()
	pinned := insertPinned(t, c, "pinned!!", pins)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(c.path(pinned.SHA256), old, old); err != nil {
		t.Fatal(err)
	}
	// Over budget, eviction takes the newer unpinned blob, not the older pinned one.
	other := insertPinned(t, c, "unpinned", nil)
	if !blobPresent(c, pinned.SHA256) || blobPresent(c, other.SHA256) {
		t.Fatalf("after eviction pinned=%v other=%v", blobPresent(c, pinned.SHA256), blobPresent(c, other.SHA256))
	}
	// Expired by age, but pinned: GC, lookup and materialization keep it.
	if _, err := c.GC(); err != nil {
		t.Fatal(err)
	}
	if missing := c.Missing([]proto.BlobRef{{SHA256: pinned.SHA256, Size: pinned.Size}}); len(missing) != 0 {
		t.Fatalf("pinned blob reported missing: %v", missing)
	}
	if hit, err := c.Materialize(context.Background(), filepath.Join(t.TempDir(), "f"), pinned, nil); err != nil || !hit {
		t.Fatalf("pinned materialize = %v, %v", hit, err)
	}
	path, ok := pins.shared()(pinned.SHA256)
	if !ok || path != c.path(pinned.SHA256) {
		t.Fatalf("shared(%s) = %q, %v", pinned.SHA256, path, ok)
	}

	pins.release()
	pins.release()
	if _, ok := pins.shared()(pinned.SHA256); ok {
		t.Fatal("released pins still share the blob")
	}
	if err := os.Chtimes(c.path(pinned.SHA256), old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GC(); err != nil {
		t.Fatal(err)
	}
	if blobPresent(c, pinned.SHA256) {
		t.Fatal("expired blob survived GC after its pin was released")
	}
}

func TestPinsCountEveryHolder(t *testing.T) {
	c := testCache(t, 1<<20, time.Hour)
	first, second := c.newPins(), c.newPins()
	e := insertPinned(t, c, "held twice", first)
	if missing, err := c.pinPresent(context.Background(), proto.Manifest{Entries: []proto.ManifestEntry{e}}, second); err != nil || len(missing) != 0 {
		t.Fatalf("pinPresent = %v, %v", missing, err)
	}
	first.release()
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(c.path(e.SHA256), old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GC(); err != nil {
		t.Fatal(err)
	}
	if !blobPresent(c, e.SHA256) {
		t.Fatal("GC removed a blob another job still pins")
	}
	second.release()
	if _, err := c.GC(); err != nil {
		t.Fatal(err)
	}
	if blobPresent(c, e.SHA256) {
		t.Fatal("expired blob survived after both pins were released")
	}
}

func TestMaterializePinsOnlyVerifiedHits(t *testing.T) {
	c := testCache(t, 1<<20, time.Hour)
	good := insertPinned(t, c, "good body", nil)
	bad := insertPinned(t, c, "bad body!", nil)
	if err := os.WriteFile(c.path(bad.SHA256), []byte("corrupt!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	pins := c.newPins()
	shared := pins.shared()
	for _, e := range []proto.ManifestEntry{good, bad} {
		if _, err := c.Materialize(context.Background(), filepath.Join(t.TempDir(), "f"), e, pins); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := shared(good.SHA256); !ok {
		t.Fatal("verified hit was not pinned")
	}
	if _, ok := shared(bad.SHA256); ok {
		t.Fatal("corrupt blob was pinned")
	}
}

func TestPinPresentReportsMissingBodiesOnce(t *testing.T) {
	c := testCache(t, 1<<20, time.Hour)
	held := insertPinned(t, c, "held", nil)
	absent := blobEntry("absent")
	twin := absent
	twin.Path = "twin"
	m := proto.Manifest{Entries: []proto.ManifestEntry{held, absent, twin}}
	pins := c.newPins()
	missing, err := c.pinPresent(context.Background(), m, pins)
	if err != nil || len(missing) != 1 || missing[0].SHA256 != absent.SHA256 {
		t.Fatalf("pinPresent missing = %v, %v", missing, err)
	}
	if _, ok := pins.shared()(held.SHA256); !ok {
		t.Fatal("present blob was not pinned")
	}
	var none *blobCache
	if missing, err := none.pinPresent(context.Background(), m, none.newPins()); err != nil || len(missing) != 2 {
		t.Fatalf("pinPresent without a cache = %v, %v", missing, err)
	}
}

// A warm job reads its whole change base from the cache, copies nothing, and
// still ships the submitted bytes of what it changed.
func TestJobBaseComesFromPinnedCache(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"a.txt": "submitted a\n", "sub/b.txt": "submitted b\n"})
	run := func(command string) string {
		t.Helper()
		var stderr bytes.Buffer
		code := client.Run(client.RunOptions{
			PeerURL: ts.URL, Root: root, Argv: []string{"/bin/sh", "-c", command},
			Stdout: io.Discard, Stderr: &stderr,
		})
		if code != 0 {
			t.Fatalf("run exit = %d; stderr: %s", code, stderr.String())
		}
		return lastJobID(t, d)
	}
	copied := func(id string) string {
		t.Helper()
		m := regexp.MustCompile(`change-base-captured","detail":"root=\S+ copied=(\d+/\d+)`).FindStringSubmatch(jobEvents(t, d, id))
		if m == nil {
			t.Fatalf("no change-base-captured event: %s", jobEvents(t, d, id))
		}
		return m[1]
	}
	// Three bodies: both files and the empty .errandignore.
	if got := copied(run("true")); got != "0/3" {
		t.Fatalf("cold job copied %s bodies, want 0/3 (inserted bodies are pinned)", got)
	}
	id := run("echo job > a.txt && rm sub/b.txt")
	if got := copied(id); got != "0/3" {
		t.Fatalf("warm job copied %s bodies, want 0/3", got)
	}
	jobDir := filepath.Join(d.jobsDir(), id)
	if _, err := os.Stat(filepath.Join(jobDir, "change-base")); !os.IsNotExist(err) {
		t.Fatalf("settled job kept its change base: %v", err)
	}
	if n := len(d.cache.pins); n != 0 {
		t.Fatalf("settled jobs left %d blobs pinned", n)
	}
	bundle, err := changeops.Load(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	staged := t.TempDir()
	archive, err := changeops.OpenBaseArchive(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if err := changeops.ExtractBase(archive, staged, bundle, 1<<20); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"a.txt": "submitted a\n", "sub/b.txt": "submitted b\n"} {
		got, err := os.ReadFile(filepath.Join(staged, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Fatalf("base %s = %q, %v; want %q", name, got, err, want)
		}
	}
}

// A job on a persistent workspace pins the bodies workspace creation cached,
// and puts back any the cache lost from the workspace's creation tree.
func TestWorkspaceJobBaseReadsAndRefillsTheCache(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"value": "initial\n"})
	if _, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "experiment"); err != nil {
		t.Fatal(err)
	}
	run := func(command string) string {
		t.Helper()
		var stderr bytes.Buffer
		code := client.Run(client.RunOptions{PeerURL: ts.URL, Root: root, Workspace: "experiment", Argv: []string{"/bin/sh", "-c", command}, Stdout: io.Discard, Stderr: &stderr})
		if code != 0 {
			t.Fatalf("run: %d %s", code, stderr.String())
		}
		id := lastJobID(t, d)
		if events := jobEvents(t, d, id); !strings.Contains(events, "copied=0/2") {
			t.Fatalf("workspace job copied bodies: %s", events)
		}
		return id
	}
	run("echo first > value")
	// Lose every cached body, as eviction would.
	entries, err := os.ReadDir(d.cache.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(d.cache.dir, entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
	id := run("echo second > value")
	bundle, err := changeops.Load(filepath.Join(d.jobsDir(), id))
	if err != nil {
		t.Fatal(err)
	}
	archive, err := changeops.OpenBaseArchive(filepath.Join(d.jobsDir(), id))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	staged := t.TempDir()
	if err := changeops.ExtractBase(archive, staged, bundle, 1<<20); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(staged, "value")); err != nil || string(got) != "initial\n" {
		t.Fatalf("base value = %q, %v; want the creation body", got, err)
	}
}

// Without a snapshot cache a job copies every body into its private store.
func TestJobBaseWithoutCacheCopiesEveryBody(t *testing.T) {
	d, err := New(Config{StateDir: t.TempDir(), InsecureNoAuth: true, CacheDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	ts := httptest.NewServer(d.Handler())
	t.Cleanup(ts.Close)
	root := workspaceWith(t, map[string]string{"a.txt": "submitted a\n"})
	var stderr bytes.Buffer
	code := client.Run(client.RunOptions{
		PeerURL: ts.URL, Root: root, Argv: []string{"/bin/sh", "-c", "echo job > a.txt"},
		Stdout: io.Discard, Stderr: &stderr,
	})
	if code != 0 {
		t.Fatalf("run exit = %d; stderr: %s", code, stderr.String())
	}
	id := lastJobID(t, d)
	if events := jobEvents(t, d, id); !strings.Contains(events, "copied=2/2") {
		t.Fatalf("cache-less job did not copy its bodies: %s", events)
	}
	jobDir := filepath.Join(d.jobsDir(), id)
	bundle, err := changeops.Load(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := changeops.OpenBaseArchive(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	staged := t.TempDir()
	if err := changeops.ExtractBase(archive, staged, bundle, 1<<20); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(staged, "a.txt")); err != nil || string(got) != "submitted a\n" {
		t.Fatalf("base a.txt = %q, %v", got, err)
	}
}
