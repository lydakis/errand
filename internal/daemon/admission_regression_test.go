package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestSubmitRejectsMismatchedDigestHeader(t *testing.T) {
	_, ts := testDaemon(t)
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/usr/bin/true"},
		Limits: proto.DefaultLimits(),
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	p, _ := mw.CreateFormField("spec")
	json.NewEncoder(p).Encode(spec)
	mw.Close()

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v0/jobs/"+proto.NewULID(), &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Errand-Digest", strings.Repeat("0", 64))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("mismatched digest = %s, want 400", resp.Status)
	}
}

func TestRejectedUploadDoesNotBurnJobID(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"input.txt": "ok"})
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
		ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits(),
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	p, _ := mw.CreateFormField("spec")
	json.NewEncoder(p).Encode(spec)
	p, _ = mw.CreateFormField("manifest")
	json.NewEncoder(p).Encode(proto.Manifest{}) // deliberately wrong root
	mw.Close()

	id := proto.NewULID()
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v0/jobs/"+id, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed upload = %s, want 400", resp.Status)
	}

	retry := rawSubmit(t, ts.URL, id, root, []string{"/usr/bin/true"})
	if retry.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(retry.Body)
		retry.Body.Close()
		t.Fatalf("retry after rejected upload = %s, want 201: %s", retry.Status, body)
	}
	retry.Body.Close()
	waitTerminal(t, ts.URL, id)
}

type blockingReader struct {
	started chan struct{}
	release chan struct{}
	start   sync.Once
	close   sync.Once
}

func (r *blockingReader) Read(p []byte) (int, error) {
	r.start.Do(func() { close(r.started) })
	<-r.release
	return 0, context.Canceled
}

func (r *blockingReader) Close() error {
	r.close.Do(func() { close(r.release) })
	return nil
}

func TestKillDuringStagingPreventsExecution(t *testing.T) {
	root := workspaceWith(t, map[string]string{"input.txt": "ok"})
	paths, _, err := snapshot.SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshot.Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	jobDir := filepath.Join(stateDir, "jobs", proto.NewULID())
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	j := &Job{
		ID: filepath.Base(jobDir), Dir: jobDir,
		Spec: proto.Spec{
			V: proto.ProtoVersion, Argv: []string{"/usr/bin/touch", marker},
			ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits(),
		},
		Admission: proto.Admission{Method: "insecure-test"},
		state:     proto.StateStaging,
		done:      make(chan struct{}),
	}
	d := &Daemon{
		cfg: Config{StateDir: stateDir, MaxJobs: 1}, jobs: map[string]*Job{j.ID: j},
		running: map[string]*Job{}, queue: []*Job{j},
	}
	reader := &blockingReader{started: make(chan struct{}), release: make(chan struct{})}
	defer reader.Close()
	startDone := make(chan error, 1)
	go func() {
		settled, err := j.stage(d, reader, manifest)
		if err == nil && !settled {
			_, err = d.queueStaged(j)
		}
		startDone <- err
	}()
	<-reader.started
	if handled, err := d.cancelBeforeStart(context.Background(), j, "user-kill", 9); !handled || err != nil {
		t.Fatal(err)
	}
	if err := <-startDone; err != nil {
		t.Fatalf("cancelled staging = %v", err)
	}
	select {
	case <-j.done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled staging job did not finish")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("cancelled staging job executed; marker stat = %v", err)
	}
	status := j.Status()
	if status.State != proto.StateKilled || status.Result == nil || status.Result.SignalNum != 9 || !status.Result.CleanupOK {
		t.Fatalf("cancelled staging status = %+v, want durable clean kill", status)
	}
	d.mu.Lock()
	running := len(d.running)
	d.mu.Unlock()
	if running != 0 {
		t.Fatal("cancelled staging job did not release the runner slot")
	}
}

func TestKillDuringCacheInsertionPreventsExecution(t *testing.T) {
	root := workspaceWith(t, map[string]string{"input.txt": "cache me"})
	paths, _, err := snapshot.SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshot.Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	var packed bytes.Buffer
	if err := snapshot.Pack(&packed, root, manifest); err != nil {
		t.Fatal(err)
	}

	cache := testCache(t, 1<<20, time.Hour)
	if err := cache.acquireInsert(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer cache.releaseInsert()
	stateDir := t.TempDir()
	jobDir := filepath.Join(stateDir, "jobs", proto.NewULID())
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	j := &Job{
		ID: filepath.Base(jobDir), Dir: jobDir,
		Spec: proto.Spec{
			V: proto.ProtoVersion, Argv: []string{"/usr/bin/touch", marker},
			ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits(),
		},
		Admission: proto.Admission{Method: "insecure-test"},
		state:     proto.StateStaging,
		done:      make(chan struct{}),
	}
	d := &Daemon{
		cfg: Config{StateDir: stateDir, MaxJobs: 1}, jobs: map[string]*Job{j.ID: j},
		running: map[string]*Job{}, queue: []*Job{j}, cache: cache,
	}
	startDone := make(chan error, 1)
	go func() {
		settled, err := j.stage(d, io.NopCloser(bytes.NewReader(packed.Bytes())), manifest)
		if err == nil && !settled {
			_, err = d.queueStaged(j)
		}
		startDone <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		events, _ := os.ReadFile(filepath.Join(j.Dir, "events.ndjson"))
		if strings.Contains(string(events), "workspace-extracted") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job did not reach cache insertion")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if handled, err := d.cancelBeforeStart(context.Background(), j, "user-kill", 9); !handled || err != nil {
		t.Fatal(err)
	}
	select {
	case <-j.done:
	case <-time.After(2 * time.Second):
		t.Fatal("cache insertion ignored staging cancellation")
	}
	if err := <-startDone; err != nil {
		t.Fatalf("canceled start = %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("cancelled cache-staging job executed; marker stat = %v", err)
	}
	status := j.Status()
	if status.State != proto.StateKilled || status.Result == nil || status.Result.SignalNum != 9 || !status.Result.CleanupOK {
		t.Fatalf("cancelled cache-staging status = %+v, want durable clean kill", status)
	}
	if running := func() int {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.running)
	}(); running != 0 {
		t.Fatal("cancelled cache-staging job did not release the runner slot")
	}
}

func TestKillCannotBeAcknowledgedAfterStartRejected(t *testing.T) {
	stateDir := t.TempDir()
	jobDir := filepath.Join(stateDir, "jobs", proto.NewULID())
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	j := &Job{
		ID: filepath.Base(jobDir), Dir: jobDir,
		Spec: proto.Spec{
			V: proto.ProtoVersion, Argv: []string{"/usr/bin/true"},
			Limits: proto.DefaultLimits(),
		},
		Admission: proto.Admission{Method: "insecure-test"},
		state:     proto.StateStaging,
		done:      make(chan struct{}),
		logReady:  make(chan struct{}),
	}
	d := &Daemon{
		cfg: Config{StateDir: stateDir, MaxJobs: 1}, jobs: map[string]*Job{j.ID: j},
		running: map[string]*Job{}, queue: []*Job{j},
	}
	_, startErr := j.stage(d, io.NopCloser(strings.NewReader("not a tar archive")), proto.Manifest{})
	if startErr == nil {
		t.Fatal("malformed staging input unexpectedly started")
	}
	if res := j.settleStartFailure(); res != nil {
		t.Fatal("rejected start unexpectedly had a cancellation result")
	}
	if handled, err := d.cancelBeforeStart(context.Background(), j, "user-kill", 9); !handled || err == nil {
		t.Fatal("kill was acknowledged after start had already rejected the job")
	}
	if err := d.abortAdmission(j, startErr); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Fatalf("rejected admission directory remains: %v", err)
	}
}

func TestKillCompletedJobIsRejected(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	id := proto.NewULID()
	resp := rawSubmit(t, ts.URL, id, root, []string{"/usr/bin/true"})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("submit = %s: %s", resp.Status, body)
	}
	resp.Body.Close()
	waitTerminal(t, ts.URL, id)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v0/jobs/"+id+"/kill?force=1", nil)
	got, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got.Body.Close()
	if got.StatusCode != http.StatusConflict {
		t.Fatalf("kill completed job = %s, want 409", got.Status)
	}
}

func TestUnsafeWorkdirRejectedBeforeAdmission(t *testing.T) {
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/usr/bin/true"}, Workdir: "../escape",
		Limits: proto.DefaultLimits(),
	}
	if err := validateSpec(spec, proto.DefaultLimits()); err == nil {
		t.Fatal("unsafe workdir passed validation")
	}
}

func TestInvalidGitCommitRejectedBeforeAdmission(t *testing.T) {
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/usr/bin/true"}, GitCommit: "not-a-full-git-object-id",
		Limits: proto.DefaultLimits(),
	}
	if err := validateSpec(spec, proto.DefaultLimits()); err == nil {
		t.Fatal("invalid git commit passed validation")
	}
}

func TestEnvironmentProvenanceIsRequired(t *testing.T) {
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/usr/bin/true"},
		Env: map[string]string{"API_TOKEN": "secret"}, Limits: proto.DefaultLimits(),
	}
	if err := validateSpec(spec, proto.DefaultLimits()); err == nil || !strings.Contains(err.Error(), "missing provenance") {
		t.Fatalf("missing environment provenance error = %v", err)
	}
}

func TestResultWriteFailureIsAmbiguous(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	j := &Job{ID: proto.NewULID(), Dir: dir, state: proto.StateRunning, done: make(chan struct{})}
	d := &Daemon{jobs: map[string]*Job{j.ID: j}, running: map[string]*Job{j.ID: j}}
	code := 0
	j.finalize(d, &proto.Result{ExitCode: &code, OutputsOK: true, LogsComplete: true}, false)
	if got := j.Status().State; got != StateAmbiguous {
		t.Fatalf("state after result write failure = %q, want %q", got, StateAmbiguous)
	}
}

func TestFinalizeRemovesWorkspaceWithRestrictiveDirectoryModes(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	readonly := filepath.Join(workspace, "readonly")
	if err := os.MkdirAll(readonly, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readonly, "file.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readonly, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readonly, 0o700) })

	id := proto.NewULID()
	j := &Job{ID: id, Dir: dir, state: proto.StateRunning, done: make(chan struct{})}
	d := &Daemon{jobs: map[string]*Job{id: j}, running: map[string]*Job{id: j}}
	code := 0
	j.finalize(d, &proto.Result{
		ExitCode: &code, OutputsOK: true, LogsComplete: true, CleanupOK: true,
	}, false)

	status := j.Status()
	if status.Result == nil || !status.Result.CleanupOK {
		t.Fatalf("cleanup result = %+v, want cleanup_ok", status.Result)
	}
	if _, err := os.Lstat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists after cleanup: %v", err)
	}
}

func TestAbortAdmissionRemovesWorkspaceWithRestrictiveDirectoryModes(t *testing.T) {
	stateDir := t.TempDir()
	id := proto.NewULID()
	jobDir := filepath.Join(stateDir, "jobs", id)
	readonly := filepath.Join(jobDir, "workspace", "readonly")
	if err := os.MkdirAll(readonly, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readonly, "file.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readonly, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readonly, 0o700) })

	j := &Job{ID: id, Dir: jobDir, state: proto.StateStaging, done: make(chan struct{}), logReady: make(chan struct{})}
	d := &Daemon{cfg: Config{StateDir: stateDir}, jobs: map[string]*Job{id: j}, running: map[string]*Job{id: j}}
	if err := d.abortAdmission(j, errors.New("start rejected")); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(jobDir); !os.IsNotExist(err) {
		t.Fatalf("rejected job directory still exists after cleanup: %v", err)
	}
	d.mu.Lock()
	_, retained := d.jobs[id]
	running := len(d.running)
	d.mu.Unlock()
	if retained || running != 0 {
		t.Fatalf("rejected job remained admitted: retained=%v running=%v", retained, running != 0)
	}
}

func TestAbortAdmissionRetainsAmbiguousJobWhenCleanupFails(t *testing.T) {
	stateDir := t.TempDir()
	jobsDir := filepath.Join(stateDir, "jobs")
	id := proto.NewULID()
	jobDir := filepath.Join(jobsDir, id)
	if err := os.MkdirAll(filepath.Join(jobDir, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(jobsDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(jobsDir, 0o700) })

	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/usr/bin/true"},
		ManifestRoot: proto.Manifest{}.RootHash(), Limits: proto.DefaultLimits(),
	}
	j := &Job{
		ID: id, Dir: jobDir, Spec: spec, Admission: proto.Admission{Method: "insecure-test"},
		state: proto.StateStaging, done: make(chan struct{}), logReady: make(chan struct{}),
	}
	if err := j.writeJSON("spec.json", proto.NewReceiptSpec(spec)); err != nil {
		t.Fatal(err)
	}
	if err := j.writeJSON("admission.json", j.Admission); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{cfg: Config{StateDir: stateDir}, jobs: map[string]*Job{id: j}, running: map[string]*Job{id: j}}
	cleanupErr := d.abortAdmission(j, errors.New("start rejected"))
	if cleanupErr == nil {
		t.Fatal("failed admission rollback discarded its cleanup failure")
	}

	d.mu.Lock()
	retained := d.jobs[id]
	running := len(d.running)
	d.mu.Unlock()
	status := j.Status()
	if retained != j || running != 0 || status.State != proto.StateAmbiguous ||
		status.Result == nil || status.Result.CleanupOK || status.Result.TransactionError == "" {
		t.Fatalf("failed rollback state: retained=%v running=%v status=%+v", retained == j, running != 0, status)
	}

	if err := os.Chmod(jobsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	d2, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	d2.mu.Lock()
	reloaded := d2.jobs[id].Status()
	d2.mu.Unlock()
	if reloaded.State != proto.StateAmbiguous || reloaded.Result == nil || reloaded.Result.CleanupOK ||
		reloaded.Result.StartError == "" || reloaded.Result.TransactionError == "" {
		t.Fatalf("failed rollback receipt after restart = %+v", reloaded)
	}
}

func TestExecutionContextIsPersisted(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	id := proto.NewULID()
	resp := rawSubmit(t, ts.URL, id, root, []string{"/usr/bin/true"})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("submit = %s: %s", resp.Status, body)
	}
	resp.Body.Close()
	waitTerminal(t, ts.URL, id)

	d.mu.Lock()
	dir := d.jobs[id].Dir
	d.mu.Unlock()
	var execution struct {
		Path string   `json:"path"`
		Argv []string `json:"argv"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &execution); err != nil {
		t.Fatal(err)
	}
	if execution.Path == "" || len(execution.Argv) == 0 {
		t.Fatalf("incomplete execution context: %+v", execution)
	}
}

func TestLoadExistingRejectsNonCurrentReceiptShape(t *testing.T) {
	stateDir := t.TempDir()
	id := proto.NewULID()
	dir := filepath.Join(stateDir, "jobs", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"tool"},
		ManifestRoot: proto.Manifest{}.RootHash(), Limits: proto.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "spec.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if d, err := New(Config{StateDir: stateDir, InsecureNoAuth: true}); err == nil {
		d.Close()
		t.Fatal("raw request spec was accepted as a current receipt")
	}
}

func TestLoadExistingRejectsInvalidCurrentResult(t *testing.T) {
	stateDir := t.TempDir()
	id := proto.NewULID()
	dir := filepath.Join(stateDir, "jobs", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"tool"},
		ManifestRoot: proto.Manifest{}.RootHash(), Limits: proto.DefaultLimits(),
	}
	if err := replaceJSON(filepath.Join(dir, "spec.json"), proto.NewReceiptSpec(spec)); err != nil {
		t.Fatal(err)
	}
	if err := replaceJSON(filepath.Join(dir, "result.json"), proto.Result{State: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if d, err := New(Config{StateDir: stateDir, InsecureNoAuth: true}); err == nil {
		d.Close()
		t.Fatal("invalid current result was accepted")
	}
}

func TestReceiptRedactsEnvironmentValues(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	secret := "receipt-secret-value"
	pathSecret := "receipt-secret-path-value"
	code := client.Run(client.RunOptions{
		PeerURL: ts.URL, Root: root, Argv: []string{"/usr/bin/true"},
		Env:    map[string]string{"API_TOKEN": secret, "PATH": pathSecret},
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if code != 0 {
		t.Fatalf("job exit = %d", code)
	}
	d.mu.Lock()
	var job *Job
	for _, candidate := range d.jobs {
		job = candidate
	}
	d.mu.Unlock()
	if job == nil {
		t.Fatal("job receipt not found")
	}
	raw, err := os.ReadFile(filepath.Join(job.Dir, "spec.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("receipt contains a verbatim environment secret")
	}
	job.mu.Lock()
	requestDigest := job.RequestDigest
	job.mu.Unlock()
	if !bytes.Contains(raw, []byte(`"API_TOKEN"`)) || !bytes.Contains(raw, []byte(`"literal"`)) {
		t.Fatalf("receipt omitted redacted environment metadata: %s", raw)
	}
	if bytes.Contains(raw, []byte(`"request_digest"`)) || bytes.Contains(raw, []byte(requestDigest)) {
		t.Fatalf("receipt contains a digest derived from environment values: %s", raw)
	}
	pathSum := sha256.Sum256([]byte(pathSecret))
	if err := filepath.WalkDir(job.Dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(contents, []byte(secret)) || bytes.Contains(contents, []byte(pathSecret)) ||
			bytes.Contains(contents, []byte(fmt.Sprintf("%x", pathSum))) ||
			bytes.Contains(contents, []byte(requestDigest)) {
			t.Errorf("receipt file %s contains an environment value or value-derived digest", filepath.Base(path))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	job.mu.Lock()
	_, retainedInMemory := job.Spec.Env["API_TOKEN"]
	job.mu.Unlock()
	if retainedInMemory {
		t.Fatal("completed job retained the environment secret in its runtime spec")
	}
	if got := job.Status().Digest; got != "" {
		t.Fatalf("status exposed secret-derived request digest %q", got)
	}
}

func TestProcessStartErrorDoesNotExposeDeclaredPATH(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"secret-dir/tool": "not an executable format\n"})
	if err := os.Chmod(filepath.Join(root, "secret-dir", "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	paths, _, err := snapshot.SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshot.Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"tool"},
		Env: map[string]string{"PATH": "secret-dir"}, EnvSources: map[string]string{"PATH": "literal"},
		ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits(),
	}
	id := proto.NewULID()
	resp := rawSubmitSpec(t, ts.URL, id, root, spec, manifest)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start failure admission = %s: %s", resp.Status, body)
	}
	if bytes.Contains(body, []byte("secret-dir")) {
		t.Fatalf("admission response exposed declared PATH material: %s", body)
	}
	status := waitTerminal(t, ts.URL, id)
	if status.Result == nil || status.Result.StartError == "" {
		t.Fatalf("start failure result = %+v", status.Result)
	}
	if strings.Contains(status.Result.StartError, "secret-dir") {
		t.Fatalf("start error exposed declared PATH material: %s", status.Result.StartError)
	}
	if !strings.Contains(status.Result.StartError, "exec format error") {
		t.Fatalf("start error lost the path-free OS cause: %s", status.Result.StartError)
	}
}

func TestExecutableResolutionUsesEffectiveJobPATH(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"tools/errand-path-probe": "#!/bin/sh\necho effective-path\n"})
	if err := os.Chmod(filepath.Join(root, "tools", "errand-path-probe"), 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := client.Run(client.RunOptions{
		PeerURL: ts.URL, Root: root, Argv: []string{"errand-path-probe"},
		Env: map[string]string{"PATH": "tools"}, Stdout: &stdout, Stderr: &stderr,
	})
	if code != 0 || stdout.String() != "effective-path\n" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	d.mu.Lock()
	var execution proto.ExecutionContext
	for _, job := range d.jobs {
		raw, err := os.ReadFile(filepath.Join(job.Dir, "execution.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &execution); err != nil {
			t.Fatal(err)
		}
	}
	d.mu.Unlock()
	if execution.Path != "" {
		t.Fatalf("execution receipt exposed declared PATH metadata: %+v", execution)
	}
	if _, err := resolveExecutable("sh", "", t.TempDir()); err == nil {
		t.Fatal("effective empty PATH fell back to the daemon's ambient PATH")
	}
}

func TestSignalStateSurvivesRestart(t *testing.T) {
	stateDir := t.TempDir()
	d1, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(d1.Handler())
	root := workspaceWith(t, nil)
	id := proto.NewULID()
	resp := rawSubmit(t, ts.URL, id, root, []string{"/bin/sh", "-c", "kill -SEGV $$"})
	resp.Body.Close()
	before := waitTerminal(t, ts.URL, id)
	ts.Close()
	if err := d1.Close(); err != nil {
		t.Fatal(err)
	}

	d2, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	d2.mu.Lock()
	after := d2.jobs[id].Status()
	d2.mu.Unlock()
	if before.State != after.State {
		t.Fatalf("state changed across restart: %q -> %q", before.State, after.State)
	}
}
