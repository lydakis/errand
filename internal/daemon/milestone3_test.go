package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func jobEvents(t *testing.T, d *Daemon, id string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(d.jobsDir(), id, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func lastJobID(t *testing.T, d *Daemon) string {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	last := ""
	for id := range d.jobs {
		if id > last {
			last = id
		}
	}
	if last == "" {
		t.Fatal("no jobs recorded")
	}
	return last
}

func TestSecondRunHitsSnapshotCache(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{
		"a.txt": "content a", "sub/b.txt": "content b",
	})
	runOnce := func() {
		t.Helper()
		var out, errb bytes.Buffer
		code := client.Run(client.RunOptions{
			PeerURL: ts.URL, Root: root,
			Argv:   []string{"/bin/sh", "-c", "cat a.txt sub/b.txt"},
			Stdout: &out, Stderr: &errb,
		})
		if code != 0 {
			t.Fatalf("run exit = %d; stderr: %s", code, errb.String())
		}
		if out.String() != "content acontent b" {
			t.Fatalf("run output %q", out.String())
		}
	}

	cachedCounts := func(id string) (cached, total int) {
		t.Helper()
		m := regexp.MustCompile(`cached=(\d+)/(\d+)`).FindStringSubmatch(jobEvents(t, d, id))
		if m == nil {
			t.Fatalf("no cached=N/M event for %s: %s", id, jobEvents(t, d, id))
		}
		cached, _ = strconv.Atoi(m[1])
		total, _ = strconv.Atoi(m[2])
		return cached, total
	}

	runOnce()
	first := lastJobID(t, d)
	cached, total := cachedCounts(first)
	if cached != 0 || total < 2 {
		t.Fatalf("first run cached=%d/%d, want 0/N", cached, total)
	}
	stats, err := d.cache.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Blobs < 2 {
		t.Fatalf("cache blobs after first run = %d, want >= 2", stats.Blobs)
	}

	runOnce()
	second := lastJobID(t, d)
	if second == first {
		t.Fatal("second run did not create a new job")
	}
	cached, total = cachedCounts(second)
	if cached != total || total < 2 {
		t.Fatalf("second run cached=%d/%d, want full cache hit", cached, total)
	}
}

func TestRunWithoutSnapshotPreservesCachedBlobs(t *testing.T) {
	d, ts := testDaemon(t)
	const content = "preserve me"
	sha, size := insertContent(t, d.cache, content)

	var stderr bytes.Buffer
	code := client.Run(client.RunOptions{
		PeerURL:    ts.URL,
		Root:       t.TempDir(),
		Argv:       []string{"/bin/sh", "-c", "exit 0"},
		NoSnapshot: true,
		Stdout:     io.Discard,
		Stderr:     &stderr,
	})
	if code != 0 {
		t.Fatalf("no-snapshot exit = %d; stderr: %s", code, stderr.String())
	}

	dest := filepath.Join(t.TempDir(), "materialized")
	hit, err := d.cache.Materialize(context.Background(), dest, proto.ManifestEntry{
		Path: "materialized", Type: proto.EntryFile, Mode: 0o600, Size: size, SHA256: sha,
	})
	if err != nil || !hit {
		t.Fatalf("cached blob after no-snapshot run: hit=%v err=%v", hit, err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != content {
		t.Fatalf("materialized content after no-snapshot run = %q, %v", got, err)
	}
}

func TestNegotiatedEvictionAutomaticallyRetriesFullSnapshot(t *testing.T) {
	d, err := New(Config{StateDir: t.TempDir(), InsecureNoAuth: true, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	base := d.Handler()
	const content = "eviction fallback content"
	sum := sha256.Sum256([]byte(content))
	sha := hex.EncodeToString(sum[:])
	var evictNext atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/snapshot/diff" || !evictNext.CompareAndSwap(true, false) {
			base.ServeHTTP(w, r)
			return
		}
		recorded := httptest.NewRecorder()
		base.ServeHTTP(recorded, r)
		d.cache.remove(sha)
		for key, values := range recorded.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(recorded.Code)
		w.Write(recorded.Body.Bytes())
	}))
	t.Cleanup(ts.Close)
	root := workspaceWith(t, map[string]string{"file.txt": content})

	run := func() (string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := client.Run(client.RunOptions{
			PeerURL: ts.URL, Root: root, Argv: []string{"/bin/cat", "file.txt"},
			Stdout: &stdout, Stderr: &stderr,
		})
		if code != 0 {
			t.Fatalf("run exit = %d; stderr: %s", code, stderr.String())
		}
		return stdout.String(), stderr.String()
	}
	if stdout, _ := run(); stdout != content {
		t.Fatalf("first run output = %q", stdout)
	}
	evictNext.Store(true)
	stdout, stderr := run()
	if stdout != content || !strings.Contains(stderr, "re-shipping the full snapshot") {
		t.Fatalf("fallback run output=%q stderr=%q", stdout, stderr)
	}
	d.mu.Lock()
	jobs := len(d.jobs)
	d.mu.Unlock()
	if jobs != 2 {
		t.Fatalf("fallback left %d admitted jobs, want one job per run", jobs)
	}
}

func TestSnapshotDiffReportsOnlyMissing(t *testing.T) {
	d, ts := testDaemon(t)
	sha, size := insertContent(t, d.cache, "known blob")
	unknown := strings.Repeat("ab", 32)
	body, _ := json.Marshal(proto.SnapshotDiffRequest{Blobs: []proto.BlobRef{
		{SHA256: sha, Size: size}, {SHA256: unknown, Size: 9},
	}})
	resp, err := http.Post(ts.URL+"/v0/snapshot/diff", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var diff proto.SnapshotDiffResponse
	if err := json.NewDecoder(resp.Body).Decode(&diff); err != nil {
		t.Fatal(err)
	}
	if len(diff.Missing) != 1 || diff.Missing[0] != unknown {
		t.Fatalf("diff missing = %v, want only the unknown hash", diff.Missing)
	}
}

func TestPartialShipWithColdCacheFailsRetryablyThenFullShipWorks(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"a.txt": "content a", "b.txt": "content b"})
	paths, _, _, err := snapshot.SelectFilesWithOptions(root, snapshot.SelectOptions{IncludeAll: true})
	if err != nil {
		t.Fatal(err)
	}
	m, err := snapshot.Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{
		Argv:         []string{"/bin/sh", "-c", "true"},
		ManifestRoot: m.RootHash(), Limits: proto.DefaultLimits(), ChangeClientID: testChangeClientID,
	}
	id := proto.NewULID()

	submitWith := func(shipFile func(proto.ManifestEntry) bool) (*http.Response, string) {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		p, _ := mw.CreateFormField("spec")
		json.NewEncoder(p).Encode(spec)
		p, _ = mw.CreateFormField("manifest")
		json.NewEncoder(p).Encode(m)
		p, _ = mw.CreateFormFile("workspace", "workspace.tar")
		if err := snapshot.PackPartial(p, root, m, shipFile); err != nil {
			t.Fatal(err)
		}
		mw.Close()
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v0/jobs/"+id, &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp, string(raw)
	}

	resp, body := submitWith(func(proto.ManifestEntry) bool { return false })
	var apiErr proto.APIError
	if err := json.Unmarshal([]byte(body), &apiErr); err != nil {
		t.Fatalf("decoding cache-miss response: %v; body=%s", err, body)
	}
	if resp.StatusCode != http.StatusConflict || apiErr.Code != proto.ErrorCodeSnapshotCacheMiss {
		t.Fatalf("partial cold-cache submit = %s: %s", resp.Status, body)
	}

	resp, body = submitWith(nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("full-ship retry = %s: %s", resp.Status, body)
	}
	waitTerminal(t, ts.URL, id)
}

func TestStorageStatsAndCacheGCEndpoints(t *testing.T) {
	d, ts := testDaemon(t)
	insertContent(t, d.cache, "blob one")
	insertContent(t, d.cache, "blob two!")

	var stats proto.StorageStats
	resp, err := http.Get(ts.URL + "/v0/storage")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&stats)
	resp.Body.Close()
	if stats.Cache == nil || stats.Cache.Blobs != 2 ||
		stats.Cache.Bytes != int64(len("blob one")+len("blob two!")) {
		t.Fatalf("storage stats = %+v", stats)
	}

	d.cache.mu.Lock()
	d.cache.ttl = 0
	d.cache.mu.Unlock()
	gcResp, err := http.Post(ts.URL+"/v0/cache/gc", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var result proto.CacheGCResult
	json.NewDecoder(gcResp.Body).Decode(&result)
	gcResp.Body.Close()
	if result.RemovedBlobs != 2 {
		t.Fatalf("gc over http = %+v", result)
	}
}

func TestCacheGCCancellationStopsHandler(t *testing.T) {
	d, _ := testDaemon(t)
	if err := d.cache.acquireInsert(context.Background()); err != nil {
		t.Fatal(err)
	}
	locked := true
	release := func() {
		if locked {
			d.cache.releaseInsert()
			locked = false
		}
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v0/cache/gc", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		d.handleCacheGC(httptest.NewRecorder(), req, Identity{})
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		release()
		<-done
		t.Fatal("cache GC handler did not stop after its request was canceled")
	}
}

func TestCacheReadCancellationStopsHandlers(t *testing.T) {
	for name, tc := range map[string]struct {
		path    string
		body    string
		handler handlerFunc
	}{
		"snapshot diff": {
			path: "/v0/snapshot/diff", body: `{"blobs":[]}`,
		},
		"storage stats": {path: "/v0/storage"},
	} {
		t.Run(name, func(t *testing.T) {
			d, _ := testDaemon(t)
			if name == "snapshot diff" {
				tc.handler = d.handleSnapshotDiff
			} else {
				tc.handler = d.handleStorageStats
			}
			d.cache.mu.Lock()
			locked := true
			release := func() {
				if locked {
					d.cache.mu.Unlock()
					locked = false
				}
			}
			defer release()

			ctx, cancel := context.WithCancel(context.Background())
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)).WithContext(ctx)
			done := make(chan struct{})
			go func() {
				tc.handler(httptest.NewRecorder(), req, Identity{})
				close(done)
			}()
			cancel()

			select {
			case <-done:
			case <-time.After(time.Second):
				release()
				<-done
				t.Fatalf("%s handler did not stop after its request was canceled", name)
			}
		})
	}
}

func TestRestartReconcilesJobsBeforeCacheMaintenance(t *testing.T) {
	stateDir := t.TempDir()
	id := proto.NewULID()
	jobDir := filepath.Join(stateDir, "jobs", id)
	workspace := filepath.Join(jobDir, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{Argv: []string{"/bin/true"}, Limits: proto.DefaultLimits()}
	raw, err := json.Marshal(proto.NewReceiptSpec(spec))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "spec.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	badCacheEntry := filepath.Join(stateDir, "cache", "blobs", "not-a-blob")
	if err := os.MkdirAll(filepath.Dir(badCacheEntry), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badCacheEntry, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	d, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if d != nil {
		d.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "opening snapshot cache") {
		t.Fatalf("New() error = %v, want cache initialization failure", err)
	}
	if _, err := os.Lstat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace survived cache initialization failure: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(jobDir, "result.json")); err != nil {
		t.Fatalf("reconciliation did not persist a result before cache failure: %v", err)
	}
}

func TestCacheDisabledUsesFullSnapshotFlow(t *testing.T) {
	d, err := New(Config{StateDir: t.TempDir(), InsecureNoAuth: true, CacheDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	ts := httptest.NewServer(d.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/v0/snapshot/diff", "application/json", strings.NewReader(`{"blobs":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("diff with disabled cache = %s, want 404", resp.Status)
	}

	root := workspaceWith(t, map[string]string{"f.txt": "x"})
	var out, errb bytes.Buffer
	code := client.Run(client.RunOptions{
		PeerURL: ts.URL, Root: root,
		Argv:   []string{"/bin/cat", "f.txt"},
		Stdout: &out, Stderr: &errb,
	})
	if code != 0 || out.String() != "x" {
		t.Fatalf("cache-less run exit=%d out=%q stderr=%s", code, out.String(), errb.String())
	}
}
