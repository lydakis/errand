package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/logio"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestLogLimitResultUsesWriterState(t *testing.T) {
	w, err := logio.NewWriter(filepath.Join(t.TempDir(), "io.log"), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.StreamWriter("stdout").Write([]byte("over limit")); err != nil {
		t.Fatal(err)
	}
	j := &Job{reaped: true}
	if got := j.limitExceeded(w); got != "log_bytes" {
		t.Fatalf("limit from writer state = %q, want log_bytes", got)
	}
}

func TestLogLimitReceiptKeepsCleanupOutcomeSeparate(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	paths, _, err := snapshot.SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshot.Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/bin/sh", "-c", "printf 0123456789"},
		ManifestRoot: manifest.RootHash(), Limits: proto.Limits{
			MaxLogBytes: 4, MaxRuntimeSec: 10, MaxWorkspaceBytes: proto.DefaultLimits().MaxWorkspaceBytes,
		},
	}
	id := proto.NewULID()
	resp := rawSubmitSpec(t, ts.URL, id, root, spec, manifest)
	resp.Body.Close()
	status := waitTerminal(t, ts.URL, id)
	if status.Result == nil || status.Result.LimitExceeded != "log_bytes" ||
		status.Result.LogsComplete || !status.Result.CleanupOK {
		t.Fatalf("log-limit result did not separate limit/log/cleanup outcomes: %+v", status.Result)
	}
}

func testDaemon(t *testing.T) (*Daemon, *httptest.Server) {
	t.Helper()
	d, err := New(Config{StateDir: t.TempDir(), InsecureNoAuth: true, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(d.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = d.Close() })
	return d, ts
}

func workspaceWith(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		abs := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(abs), 0o755)
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// rawSubmit drives the wire protocol directly so tests control the job ID.
func rawSubmit(t *testing.T, url, id, root string, argv []string) *http.Response {
	t.Helper()
	paths, _, err := snapshot.SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	m, err := snapshot.Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: argv,
		ManifestRoot: m.RootHash(), Limits: proto.DefaultLimits(),
	}
	return rawSubmitSpec(t, url, id, root, spec, m)
}

func rawSubmitSpec(t *testing.T, url, id, root string, spec proto.Spec, m proto.Manifest) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	p, _ := mw.CreateFormField("spec")
	json.NewEncoder(p).Encode(spec)
	p, _ = mw.CreateFormField("manifest")
	json.NewEncoder(p).Encode(m)
	p, _ = mw.CreateFormFile("workspace", "workspace.tar")
	if err := snapshot.Pack(p, root, m); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req, _ := http.NewRequest(http.MethodPut, url+"/v0/jobs/"+id, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Errand-Digest", spec.Digest())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func waitTerminal(t *testing.T, url, id string) proto.JobStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url + "/v0/jobs/" + id)
		if err != nil {
			t.Fatal(err)
		}
		var st proto.JobStatus
		json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		if st.State != proto.StateRunning {
			return st
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("job never reached a terminal state")
	return proto.JobStatus{}
}

func TestRunHappyPath(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"hello.txt": "hello from the workspace\n"})
	var stdout, stderr bytes.Buffer
	code := client.Run(client.RunOptions{
		PeerURL: ts.URL, Root: root,
		Argv:   []string{"/bin/sh", "-c", "cat hello.txt; echo oops >&2; exit 3"},
		Stdout: &stdout, Stderr: &stderr,
	})
	if code != 3 {
		t.Fatalf("exit code = %d, want the remote 3; stderr: %s", code, stderr.String())
	}
	if stdout.String() != "hello from the workspace\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "oops") {
		t.Fatalf("stderr missing remote output: %q", stderr.String())
	}
}

func TestEnvIsExplicitOnly(t *testing.T) {
	_, ts := testDaemon(t)
	t.Setenv("ERRAND_TEST_SECRET", "leaky")
	root := workspaceWith(t, nil)
	var stdout bytes.Buffer
	code := client.Run(client.RunOptions{
		PeerURL: ts.URL, Root: root,
		Argv:   []string{"/bin/sh", "-c", `echo "secret=[$ERRAND_TEST_SECRET] set=[$DECLARED]"`},
		Env:    map[string]string{"DECLARED": "yes"},
		Stdout: &stdout, Stderr: &bytes.Buffer{},
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != "secret=[] set=[yes]" {
		t.Fatalf("environment leaked or dropped: %q", got)
	}
}

func TestAtMostOnceAdmission(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"f": "x"})
	id := proto.NewULID()

	resp1 := rawSubmit(t, ts.URL, id, root, []string{"/bin/echo", "once"})
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first submit: %s", resp1.Status)
	}
	resp1.Body.Close()
	waitTerminal(t, ts.URL, id)

	// Identical retry returns the existing job, no re-execution.
	resp2 := rawSubmit(t, ts.URL, id, root, []string{"/bin/echo", "once"})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("idempotent retry: %s, want 200", resp2.Status)
	}
	resp2.Body.Close()

	// Same ID, different request: refused.
	resp3 := rawSubmit(t, ts.URL, id, root, []string{"/bin/echo", "DIFFERENT"})
	if resp3.StatusCode != http.StatusConflict {
		t.Fatalf("digest mismatch: %s, want 409", resp3.Status)
	}
	resp3.Body.Close()
}

func TestEnvironmentBearingRetrySameDaemon(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	paths, _, err := snapshot.SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshot.Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/usr/bin/true"},
		Env: map[string]string{"PIN": "0427"}, EnvSources: map[string]string{"PIN": "literal"},
		ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits(),
	}
	id := proto.NewULID()
	resp := rawSubmitSpec(t, ts.URL, id, root, spec, manifest)
	resp.Body.Close()
	waitTerminal(t, ts.URL, id)

	resp = rawSubmitSpec(t, ts.URL, id, root, spec, manifest)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("same-daemon environment retry = %s, want 200", resp.Status)
	}
	resp.Body.Close()
}

func TestEnvironmentBearingRetryFailsClosedAfterRestart(t *testing.T) {
	stateDir := t.TempDir()
	d1, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	ts1 := httptest.NewServer(d1.Handler())
	root := workspaceWith(t, nil)
	paths, _, err := snapshot.SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshot.Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/usr/bin/true"},
		Env: map[string]string{"PIN": "0427"}, EnvSources: map[string]string{"PIN": "literal"},
		ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits(),
	}
	id := proto.NewULID()
	resp := rawSubmitSpec(t, ts1.URL, id, root, spec, manifest)
	resp.Body.Close()
	waitTerminal(t, ts1.URL, id)
	ts1.Close()
	if err := d1.Close(); err != nil {
		t.Fatal(err)
	}

	d2, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	ts2 := httptest.NewServer(d2.Handler())
	defer ts2.Close()
	resp = rawSubmitSpec(t, ts2.URL, id, root, spec, manifest)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("post-restart environment retry = %s, want 409", resp.Status)
	}
	resp.Body.Close()

	d2.mu.Lock()
	loaded := d2.jobs[id]
	d2.mu.Unlock()
	if loaded == nil || loaded.RequestDigest != "" {
		t.Fatalf("restarted job retained secret-derived request identity: %+v", loaded)
	}
}

func TestEnvironmentlessRetrySurvivesRestart(t *testing.T) {
	stateDir := t.TempDir()
	d1, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	ts1 := httptest.NewServer(d1.Handler())
	root := workspaceWith(t, nil)
	id := proto.NewULID()
	resp := rawSubmit(t, ts1.URL, id, root, []string{"/usr/bin/true"})
	resp.Body.Close()
	waitTerminal(t, ts1.URL, id)
	ts1.Close()
	if err := d1.Close(); err != nil {
		t.Fatal(err)
	}

	d2, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	ts2 := httptest.NewServer(d2.Handler())
	defer ts2.Close()
	resp = rawSubmit(t, ts2.URL, id, root, []string{"/usr/bin/true"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-restart environment-free retry = %s, want 200", resp.Status)
	}
	resp.Body.Close()
}

func TestTerminalLogReplayAlwaysPrecedesStatus(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	id := proto.NewULID()
	resp := rawSubmit(t, ts.URL, id, root, []string{"/bin/echo", "persisted-log"})
	resp.Body.Close()
	waitTerminal(t, ts.URL, id)
	for i := 0; i < 20; i++ {
		resp, err := http.Get(ts.URL + "/v0/jobs/" + id + "/logs?follow=1&from=0")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		logAt := bytes.Index(body, []byte("event: log"))
		statusAt := bytes.Index(body, []byte("event: status"))
		if logAt < 0 || statusAt < 0 || logAt > statusAt {
			t.Fatalf("terminal replay %d omitted or reordered logs: %s", i, body)
		}
	}
}

func TestMissingTerminalLogProducesStreamError(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	id := proto.NewULID()
	resp := rawSubmit(t, ts.URL, id, root, []string{"/bin/echo", "persisted-log"})
	resp.Body.Close()
	waitTerminal(t, ts.URL, id)
	d.mu.Lock()
	logPath := filepath.Join(d.jobs[id].Dir, "io.log")
	d.mu.Unlock()
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(ts.URL + "/v0/jobs/" + id + "/logs?follow=1&from=0")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("event: error")) || bytes.Contains(body, []byte("event: status")) {
		t.Fatalf("missing terminal log response = %s", body)
	}
}

func TestLogFileErrorRetryClassification(t *testing.T) {
	if retryableLogFileError(os.ErrNotExist) {
		t.Fatal("missing persisted logs must be a permanent integrity failure")
	}
	if retryableLogFileError(syscall.EISDIR) || retryableLogFileError(syscall.ELOOP) ||
		retryableLogFileError(os.ErrPermission) {
		t.Fatal("stable persisted-log filesystem failures must be permanent")
	}
	if !retryableLogFileError(syscall.EIO) {
		t.Fatal("transient persisted-log I/O failure must be retryable")
	}
}

func TestDirectoryAtTerminalLogPathProducesPermanentStreamError(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	id := proto.NewULID()
	resp := rawSubmit(t, ts.URL, id, root, []string{"/bin/echo", "persisted-log"})
	resp.Body.Close()
	waitTerminal(t, ts.URL, id)
	d.mu.Lock()
	logPath := filepath.Join(d.jobs[id].Dir, "io.log")
	d.mu.Unlock()
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(logPath, 0o700); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(ts.URL + "/v0/jobs/" + id + "/logs?follow=1&from=0")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("event: error")) ||
		!bytes.Contains(body, []byte(`"retryable":false`)) {
		t.Fatalf("invalid terminal log response = %s", body)
	}
}

func TestBusyAndForceKill(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	longID := proto.NewULID()
	resp := rawSubmit(t, ts.URL, longID, root, []string{"/bin/sh", "-c", "sleep 30"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("long job: %s", resp.Status)
	}
	resp.Body.Close()

	resp2 := rawSubmit(t, ts.URL, proto.NewULID(), root, []string{"/bin/echo", "hi"})
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second concurrent job: %s, want 429 busy", resp2.Status)
	}
	resp2.Body.Close()

	kill, _ := http.NewRequest(http.MethodPost, ts.URL+"/v0/jobs/"+longID+"/kill?force=1", nil)
	kr, err := http.DefaultClient.Do(kill)
	if err != nil {
		t.Fatal(err)
	}
	kr.Body.Close()
	st := waitTerminal(t, ts.URL, longID)
	if st.Result == nil || st.Result.Signal == "" {
		t.Fatalf("force-killed job should record a signal, got %+v", st.Result)
	}

	// Slot released: a new job runs.
	afterID := proto.NewULID()
	resp3 := rawSubmit(t, ts.URL, afterID, root, []string{"/bin/echo", "hi"})
	if resp3.StatusCode != http.StatusCreated {
		t.Fatalf("after kill, submit: %s", resp3.Status)
	}
	resp3.Body.Close()
	waitTerminal(t, ts.URL, afterID)
}

func TestAmbiguousAfterRestart(t *testing.T) {
	stateDir := t.TempDir()
	d1, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(d1.Handler())
	defer ts.Close()
	root := workspaceWith(t, nil)
	id := proto.NewULID()
	resp := rawSubmit(t, ts.URL, id, root, []string{"/bin/sh", "-c", "sleep 30"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("submit: %s", resp.Status)
	}
	resp.Body.Close()

	// A second daemon over the same state must report the unfinished job
	// as ambiguous — and must not replay it.
	if err := d1.Close(); err != nil {
		t.Fatal(err)
	}
	d2, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	d2.mu.Lock()
	j := d2.jobs[id]
	d2.mu.Unlock()
	if j == nil || j.state != StateAmbiguous {
		t.Fatalf("restarted daemon sees state %v, want ambiguous", j)
	}

	kill, _ := http.NewRequest(http.MethodPost, ts.URL+"/v0/jobs/"+id+"/kill?force=1", nil)
	if kr, err := http.DefaultClient.Do(kill); err == nil {
		kr.Body.Close()
	}
	waitTerminal(t, ts.URL, id)
}

func TestStateDirRejectsSecondDaemon(t *testing.T) {
	stateDir := t.TempDir()
	d1, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d1.Close()

	if d2, err := New(Config{StateDir: stateDir, InsecureNoAuth: true}); err == nil {
		d2.Close()
		t.Fatal("second daemon acquired an already locked state directory")
	}
}

func TestDefaultUploadLimitCoversDefaultWorkspace(t *testing.T) {
	d, err := New(Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.cfg.MaxUploadBytes <= d.cfg.MaxLimits.MaxWorkspaceBytes {
		t.Fatalf("upload cap %d must exceed workspace cap %d", d.cfg.MaxUploadBytes, d.cfg.MaxLimits.MaxWorkspaceBytes)
	}
}

func TestUploadLimitCannotUndercutWorkspaceLimit(t *testing.T) {
	limits := proto.DefaultLimits()
	if d, err := New(Config{
		StateDir: t.TempDir(), InsecureNoAuth: true,
		MaxLimits: limits, MaxUploadBytes: limits.MaxWorkspaceBytes,
	}); err == nil {
		d.Close()
		t.Fatal("daemon accepted an upload cap that cannot carry its workspace ceiling")
	}
}

func TestLimitsRejectedAboveCeiling(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	paths, _, _ := snapshot.SelectFiles(root)
	m, _ := snapshot.Build(root, paths)
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/bin/true"},
		ManifestRoot: m.RootHash(),
		Limits:       proto.Limits{MaxLogBytes: 1 << 60, MaxRuntimeSec: 1, MaxWorkspaceBytes: 1},
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	p, _ := mw.CreateFormField("spec")
	json.NewEncoder(p).Encode(spec)
	mw.Close()
	req, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/v0/jobs/%s", ts.URL, proto.NewULID()), &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("over-ceiling limits: %s, want 400", resp.Status)
	}
}
