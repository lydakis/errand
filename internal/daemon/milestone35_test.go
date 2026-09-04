package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func concurrencyDaemon(t *testing.T, maxJobs, maxQueued int) (*Daemon, *httptest.Server) {
	t.Helper()
	d, err := New(Config{
		StateDir: t.TempDir(), InsecureNoAuth: true, Version: "test",
		MaxJobs: maxJobs, MaxQueued: maxQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	ts := httptest.NewServer(d.Handler())
	t.Cleanup(ts.Close)
	return d, ts
}

func waitState(t *testing.T, url, id, want string) {
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
		if st.State == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("job %s never reached state %q", id, want)
}

func forceKill(t *testing.T, url, id string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/v0/jobs/"+id+"/kill?force=1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func blockedSubmit(t *testing.T, url, id, root string, argv []string) (<-chan struct{}, func(), <-chan *http.Response) {
	t.Helper()
	paths, _, _, err := snapshot.SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshot.Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{
		Argv:         argv,
		ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits(), ChangeClientID: testChangeClientID,
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	release := make(chan struct{})
	workspacePart := make(chan struct{})
	go func() {
		err := func() error {
			part, err := mw.CreateFormField("spec")
			if err != nil {
				return err
			}
			if err := json.NewEncoder(part).Encode(spec); err != nil {
				return err
			}
			part, err = mw.CreateFormField("manifest")
			if err != nil {
				return err
			}
			if err := json.NewEncoder(part).Encode(manifest); err != nil {
				return err
			}
			part, err = mw.CreateFormFile("workspace", "workspace.tar")
			if err != nil {
				return err
			}
			close(workspacePart)
			<-release
			if err := snapshot.Pack(part, root, manifest); err != nil {
				return err
			}
			return mw.Close()
		}()
		_ = pw.CloseWithError(err)
	}()
	req, err := http.NewRequest(http.MethodPut, url+"/v0/jobs/"+id, pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Errand-Digest", spec.Digest())
	response := make(chan *http.Response, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			response <- nil
			return
		}
		response <- resp
	}()
	return workspacePart, func() { close(release) }, response
}

func jobStatus(t *testing.T, url, id string) proto.JobStatus {
	t.Helper()
	resp, err := http.Get(url + "/v0/jobs/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status proto.JobStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// TestJobsRunConcurrently proves two slots genuinely overlap: job A only
// exits once job B (submitted second) has created a file A waits for.
func TestJobsRunConcurrently(t *testing.T) {
	_, ts := concurrencyDaemon(t, 2, 0)
	shared := t.TempDir()
	gate := filepath.Join(shared, "go-ahead")
	root := workspaceWith(t, map[string]string{"f": "x"})

	aID := proto.NewULID()
	resp := rawSubmit(t, ts.URL, aID, root, []string{"/bin/sh", "-c",
		"while [ ! -e " + gate + " ]; do sleep 0.05; done"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("job A: %s", resp.Status)
	}
	resp.Body.Close()

	bID := proto.NewULID()
	resp = rawSubmit(t, ts.URL, bID, root, []string{"/usr/bin/touch", gate})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("job B: %s: %s", resp.Status, body)
	}
	resp.Body.Close()

	// A can only exit 0 if B ran while A was still running.
	if st := waitTerminal(t, ts.URL, aID); st.Result == nil || st.Result.ExitCode == nil || *st.Result.ExitCode != 0 {
		t.Fatalf("job A result = %+v, want exit 0 via concurrent B", st.Result)
	}
	waitTerminal(t, ts.URL, bID)
}

func TestMaxJobsIsHardUpperBound(t *testing.T) {
	_, ts := concurrencyDaemon(t, 2, 1)
	root := workspaceWith(t, nil)
	ids := []string{proto.NewULID(), proto.NewULID(), proto.NewULID()}
	for _, id := range ids[:2] {
		resp := rawSubmit(t, ts.URL, id, root, []string{"/bin/sleep", "30"})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("running submit %s = %s", id, resp.Status)
		}
		resp.Body.Close()
		waitState(t, ts.URL, id, proto.StateRunning)
	}
	resp := rawSubmit(t, ts.URL, ids[2], root, []string{"/bin/true"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("queued submit = %s", resp.Status)
	}
	resp.Body.Close()
	waitState(t, ts.URL, ids[2], proto.StateQueued)

	infoResp, err := http.Get(ts.URL + "/v0/info")
	if err != nil {
		t.Fatal(err)
	}
	var info proto.Info
	if err := json.NewDecoder(infoResp.Body).Decode(&info); err != nil {
		infoResp.Body.Close()
		t.Fatal(err)
	}
	infoResp.Body.Close()
	if !info.Busy || info.RunningJobs != 2 || info.StartingJobs != 0 || info.QueuedJobs != 1 {
		t.Fatalf("max-jobs occupancy = %+v, want two running and one queued", info)
	}

	for _, id := range ids {
		forceKill(t, ts.URL, id)
		waitTerminal(t, ts.URL, id)
	}
}

func TestAdmissionFIFOIncludesConcurrentStaging(t *testing.T) {
	_, ts := concurrencyDaemon(t, 1, 2)
	root := workspaceWith(t, map[string]string{"f": "x"})

	first := proto.NewULID()
	workspacePart, releaseFirst, firstResponse := blockedSubmit(
		t, ts.URL, first, root, []string{"/bin/sh", "-c", "exit 0"},
	)
	<-workspacePart
	waitState(t, ts.URL, first, proto.StateStaging)

	second := proto.NewULID()
	resp := rawSubmit(t, ts.URL, second, root, []string{"/bin/sleep", "30"})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		releaseFirst()
		t.Fatalf("second submit = %s: %s", resp.Status, body)
	}
	resp.Body.Close()
	if status := jobStatus(t, ts.URL, second); status.State != proto.StateQueued {
		forceKill(t, ts.URL, second)
		releaseFirst()
		t.Fatalf("later submission overtook staging head: state=%s", status.State)
	}

	releaseFirst()
	resp = <-firstResponse
	if resp == nil || resp.StatusCode != http.StatusCreated {
		if resp == nil {
			t.Fatal("first submit did not complete after staging release")
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("first submit after staging release = %s: %s", resp.Status, body)
	}
	resp.Body.Close()
	waitTerminal(t, ts.URL, first)
	forceKill(t, ts.URL, second)
	waitTerminal(t, ts.URL, second)
}

func TestStagingConsumesCapacityWhenQueueIsDisabled(t *testing.T) {
	_, ts := concurrencyDaemon(t, 1, 0)
	root := workspaceWith(t, map[string]string{"f": "x"})
	first := proto.NewULID()
	workspacePart, releaseFirst, firstResponse := blockedSubmit(
		t, ts.URL, first, root, []string{"/bin/sh", "-c", "exit 0"},
	)
	released := false
	defer func() {
		if released {
			return
		}
		releaseFirst()
		if resp := <-firstResponse; resp != nil {
			resp.Body.Close()
		}
	}()
	<-workspacePart
	waitState(t, ts.URL, first, proto.StateStaging)
	infoResp, err := http.Get(ts.URL + "/v0/info")
	if err != nil {
		t.Fatal(err)
	}
	var info proto.Info
	if err := json.NewDecoder(infoResp.Body).Decode(&info); err != nil {
		infoResp.Body.Close()
		t.Fatal(err)
	}
	infoResp.Body.Close()
	if !info.Busy || info.StagingJobs != 1 || info.StartingJobs != 0 || info.RunningJobs != 0 ||
		info.QueuedJobs != 0 || info.MaxQueued != 0 {
		t.Fatalf("staging occupancy = %+v, want busy with one staging job", info)
	}

	resp := rawSubmit(t, ts.URL, proto.NewULID(), root, []string{"/bin/sh", "-c", "exit 0"})
	if resp.StatusCode != http.StatusTooManyRequests {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		releaseFirst()
		released = true
		if firstResp := <-firstResponse; firstResp != nil {
			firstResp.Body.Close()
		}
		t.Fatalf("submit during full staging capacity = %s: %s", resp.Status, body)
	}
	resp.Body.Close()

	releaseFirst()
	released = true
	resp = <-firstResponse
	if resp == nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("first submit did not complete: %v", resp)
	}
	resp.Body.Close()
	waitTerminal(t, ts.URL, first)
}

func TestPreStartSignalWaitsForDurableSettlement(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	j := &Job{
		ID: proto.NewULID(), Dir: dir, Spec: proto.Spec{Argv: []string{"/bin/true"}},
		state: proto.StateStaging, done: make(chan struct{}), logReady: make(chan struct{}),
		stagingDone: make(chan struct{}),
	}
	close(j.stagingDone)
	d := &Daemon{
		cfg: Config{MaxJobs: 1, InsecureNoAuth: true}, jobs: map[string]*Job{j.ID: j},
		running: map[string]*Job{}, queue: []*Job{j},
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/jobs/"+j.ID+"/signal", bytes.NewBufferString(`{"signal":"SIGINT"}`))
	req.SetPathValue("id", j.ID)
	rec := httptest.NewRecorder()
	returned := make(chan struct{})
	go func() {
		d.handleSignal(rec, req, Identity{})
		close(returned)
	}()

	select {
	case <-returned:
		t.Fatal("pre-start signal was acknowledged before durable settlement")
	case <-time.After(100 * time.Millisecond):
	}

	res := j.cancelledBeforeStart()
	if res == nil {
		t.Fatal("signal did not establish pre-start cancellation")
	}
	j.finalize(d, res, true)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("signal handler did not return after durable settlement")
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("signal response = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "result.json")); err != nil {
		t.Fatalf("signal returned without a durable result: %v", err)
	}
}

func TestCancellationBetweenStageAndQueueSettlesWithoutWaitingForSlot(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	j := &Job{
		ID: proto.NewULID(), Dir: dir, Spec: proto.Spec{Argv: []string{"/bin/true"}},
		state: proto.StateStaging, done: make(chan struct{}), logReady: make(chan struct{}),
		stagingDone: make(chan struct{}),
	}
	close(j.stagingDone)
	blocker := &Job{ID: proto.NewULID(), state: proto.StateRunning}
	d := &Daemon{
		cfg: Config{MaxJobs: 1, MaxQueued: 1}, jobs: map[string]*Job{j.ID: j, blocker.ID: blocker},
		running: map[string]*Job{blocker.ID: blocker}, queue: []*Job{j},
	}
	cancelDone := make(chan error, 1)
	go func() {
		handled, err := d.cancelBeforeStart(context.Background(), j, "user-signal", syscall.SIGINT)
		if !handled && err == nil {
			err = errors.New("pre-start cancellation was not handled")
		}
		cancelDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		j.mu.Lock()
		cancelled := j.killed != ""
		j.mu.Unlock()
		if cancelled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancellation did not reach the staged job")
		}
		time.Sleep(time.Millisecond)
	}
	cancelled, err := d.queueStaged(j)
	if err != nil {
		t.Fatal(err)
	}
	if !cancelled {
		t.Fatal("cancelled job was committed to the runnable queue")
	}
	if err := <-cancelDone; err != nil {
		t.Fatal(err)
	}
	if status := j.Status(); status.State != proto.StateKilled || status.Result == nil {
		t.Fatalf("cancelled staged job = %+v, want durable killed result", status)
	}
}

func TestIdleLaunchFailureSettlesDurableReceipt(t *testing.T) {
	d, ts := concurrencyDaemon(t, 1, 1)
	root := workspaceWith(t, map[string]string{"f": "x"})
	paths, _, _, err := snapshot.SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshot.Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{
		Argv:         []string{"/definitely/not/a/real/executable"},
		Env:          map[string]string{"API_TOKEN": "launch-failure-secret"},
		EnvSources:   map[string]string{"API_TOKEN": "literal"},
		ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits(),
	}
	id := proto.NewULID()
	resp := rawSubmitSpec(t, ts.URL, id, root, spec, manifest)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("idle launch failure = %s: %s, want durable 201 receipt", resp.Status, body)
	}
	resp.Body.Close()
	status := waitTerminal(t, ts.URL, id)
	if status.State != proto.StateExited || status.Result == nil || status.Result.StartError == "" ||
		status.Result.Started || !status.Result.CleanupOK {
		t.Fatalf("idle launch failure result = %+v / %+v", status.State, status.Result)
	}
	if _, err := os.Stat(filepath.Join(d.jobsDir(), id, "result.json")); err != nil {
		t.Fatalf("idle launch failure left no durable receipt: %v", err)
	}
	d.mu.Lock()
	j := d.jobs[id]
	d.mu.Unlock()
	j.mu.Lock()
	retainedEnv := len(j.Spec.Env)
	j.mu.Unlock()
	if retainedEnv != 0 {
		t.Fatalf("idle launch failure retained %d environment values", retainedEnv)
	}
}

func TestCleanupKeepsScopeUntilQueuedEvidenceIsRemoved(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	scopePath := filepath.Join(dir, "scope.json")
	record, err := json.Marshal(scopeRecord{Token: strings.Repeat("ab", 16)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scopePath, record, 0o600); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(dir, queuedMarkerName)
	if err := os.Mkdir(markerPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(markerPath, "sentinel"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, cleanupErrs := cleanupPersistedRuntime(&Job{Dir: dir})
	if len(cleanupErrs) == 0 || !strings.Contains(strings.Join(cleanupErrs, "; "), "queued marker") {
		t.Fatalf("cleanup errors = %v, want queued-marker failure", cleanupErrs)
	}
	if _, err := os.Stat(scopePath); err != nil {
		t.Fatalf("scope evidence was removed before queued evidence: %v", err)
	}
}

func TestRestartSettlesPersistedQueuedJobAsNeverStarted(t *testing.T) {
	stateDir := t.TempDir()
	id := proto.NewULID()
	dir := filepath.Join(stateDir, "jobs", id)
	if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "change-base"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".change-base-partial"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{Argv: []string{"/bin/true"}, Limits: proto.DefaultLimits()}
	j := &Job{ID: id, Dir: dir, Spec: spec, Admission: proto.Admission{Time: time.Now()}}
	if err := j.writeJSON("spec.json", proto.NewReceiptSpec(spec)); err != nil {
		t.Fatal(err)
	}
	if err := j.writeJSON("admission.json", j.Admission); err != nil {
		t.Fatal(err)
	}
	if err := j.writeJSON("queued.json", map[string]string{"state": proto.StateQueued}); err != nil {
		t.Fatal(err)
	}

	d, err := New(Config{StateDir: stateDir, InsecureNoAuth: true, MaxJobs: 1, MaxQueued: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	status := d.jobs[id].Status()
	if status.State != proto.StateExited || status.Result == nil || status.Result.Started ||
		status.Result.StartError == "" || !strings.Contains(status.Result.StartError, "never started") ||
		!status.Result.CleanupOK || !status.Result.LogsComplete {
		t.Fatalf("reconciled queued job = %+v / %+v", status.State, status.Result)
	}
	if _, err := os.Stat(filepath.Join(dir, queuedMarkerName)); err != nil {
		t.Fatalf("queued evidence was removed before durable settlement: %v", err)
	}
	for _, name := range []string{"change-base", ".change-base-partial"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("queued restart retained %s: %v", name, err)
		}
	}
}

func TestSignalCancelsDequeuedJobBeforeStart(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	j := &Job{
		ID: proto.NewULID(), Dir: dir,
		Spec:  proto.Spec{Argv: []string{"/usr/bin/touch", marker}, Limits: proto.DefaultLimits()},
		state: proto.StateQueued,
		done:  make(chan struct{}), logReady: make(chan struct{}),
	}
	d := &Daemon{
		cfg: Config{MaxJobs: 1}, jobs: map[string]*Job{j.ID: j},
		running: map[string]*Job{j.ID: j},
	}
	cancelDone := make(chan error, 1)
	go func() {
		handled, err := d.cancelBeforeStart(context.Background(), j, "user-signal", syscall.SIGINT)
		if !handled && err == nil {
			err = errors.New("dequeued cancellation was not handled")
		}
		cancelDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		j.mu.Lock()
		cancelled := j.killed != ""
		j.mu.Unlock()
		if cancelled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancellation did not reach the dequeued job")
		}
		time.Sleep(time.Millisecond)
	}
	if err := j.launch(d); err != nil {
		t.Fatalf("cancelled launch returned an error: %v", err)
	}
	if err := <-cancelDone; err != nil {
		t.Fatal(err)
	}
	status := j.Status()
	if status.State != proto.StateKilled || status.Result == nil || status.Result.Started ||
		status.Result.SignalNum != int(syscall.SIGINT) {
		t.Fatalf("cancelled launch status = %+v", status)
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("cancelled job executed after dequeue: %v", err)
	}
}

func TestQueueIsFIFOAndDrainsOnRelease(t *testing.T) {
	_, ts := concurrencyDaemon(t, 1, 2)
	shared := t.TempDir()
	order := filepath.Join(shared, "order")
	root := workspaceWith(t, map[string]string{"f": "x"})

	blocker := proto.NewULID()
	resp := rawSubmit(t, ts.URL, blocker, root, []string{"/bin/sleep", "30"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("blocker: %s", resp.Status)
	}
	resp.Body.Close()

	first := proto.NewULID()
	resp = rawSubmit(t, ts.URL, first, root, []string{"/bin/sh", "-c", "echo first >> " + order})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first queued: %s", resp.Status)
	}
	resp.Body.Close()
	waitState(t, ts.URL, first, proto.StateQueued)

	second := proto.NewULID()
	resp = rawSubmit(t, ts.URL, second, root, []string{"/bin/sh", "-c", "echo second >> " + order})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("second queued: %s", resp.Status)
	}
	resp.Body.Close()

	// Capacity 1 running + 2 queued is now full.
	resp = rawSubmit(t, ts.URL, proto.NewULID(), root, []string{"/bin/echo", "no room"})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-capacity submit = %s, want 429", resp.Status)
	}
	resp.Body.Close()

	forceKill(t, ts.URL, blocker)
	waitTerminal(t, ts.URL, first)
	waitTerminal(t, ts.URL, second)
	got, err := os.ReadFile(order)
	if err != nil || string(got) != "first\nsecond\n" {
		t.Fatalf("queue drain order = %q, %v (want FIFO)", got, err)
	}
}

func TestKillQueuedJobSettlesWithoutRunning(t *testing.T) {
	_, ts := concurrencyDaemon(t, 1, 2)
	shared := t.TempDir()
	marker := filepath.Join(shared, "ran")
	root := workspaceWith(t, map[string]string{"f": "x"})

	blocker := proto.NewULID()
	resp := rawSubmit(t, ts.URL, blocker, root, []string{"/bin/sleep", "30"})
	resp.Body.Close()

	queued := proto.NewULID()
	resp = rawSubmit(t, ts.URL, queued, root, []string{"/usr/bin/touch", marker})
	resp.Body.Close()
	waitState(t, ts.URL, queued, proto.StateQueued)

	forceKill(t, ts.URL, queued)
	st := waitTerminal(t, ts.URL, queued)
	if st.State != proto.StateKilled || st.Result == nil || st.Result.Signal == "" || st.Result.Started {
		t.Fatalf("killed queued job = %+v / %+v", st.State, st.Result)
	}

	forceKill(t, ts.URL, blocker)
	waitTerminal(t, ts.URL, blocker)
	time.Sleep(200 * time.Millisecond) // give any wrongful launch a chance to run
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("killed queued job ran anyway: %v", err)
	}
}

// TestQueuedLaunchFailureSettlesDurableReceipt: the submitter is gone when
// a queued job's launch fails, so the failure must become a receipt, not a
// rollback.
func TestQueuedLaunchFailureSettlesDurableReceipt(t *testing.T) {
	d, ts := concurrencyDaemon(t, 1, 2)
	root := workspaceWith(t, map[string]string{"f": "x"})

	blocker := proto.NewULID()
	resp := rawSubmit(t, ts.URL, blocker, root, []string{"/bin/sleep", "30"})
	resp.Body.Close()

	doomed := proto.NewULID()
	resp = rawSubmit(t, ts.URL, doomed, root, []string{"/definitely/not/a/real/executable"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("doomed queued submit = %s, want 201 (failure is at launch)", resp.Status)
	}
	resp.Body.Close()
	waitState(t, ts.URL, doomed, proto.StateQueued)

	forceKill(t, ts.URL, blocker)
	st := waitTerminal(t, ts.URL, doomed)
	if st.Result == nil || st.Result.StartError == "" || st.Result.Started {
		t.Fatalf("doomed job result = %+v, want durable start error", st.Result)
	}
	if _, err := os.ReadFile(filepath.Join(d.jobsDir(), doomed, "result.json")); err != nil {
		t.Fatalf("queued launch failure left no durable receipt: %v", err)
	}
}

func TestClientRunsThroughQueueTransparently(t *testing.T) {
	_, ts := concurrencyDaemon(t, 1, 2)
	root := workspaceWith(t, map[string]string{"f.txt": "queued payload"})

	blocker := proto.NewULID()
	resp := rawSubmit(t, ts.URL, blocker, workspaceWith(t, nil), []string{"/bin/sleep", "30"})
	resp.Body.Close()

	done := make(chan int, 1)
	var out, errb bytes.Buffer
	var queuedID string
	go func() {
		done <- client.Run(client.RunOptions{
			PeerURL: ts.URL, Root: root,
			Argv:   []string{"/bin/sh", "-c", "cat f.txt; exit 5"},
			Stdout: &out, Stderr: &errb,
		})
	}()

	// Find the queued job and confirm the client was told.
	deadline := time.Now().Add(10 * time.Second)
	for queuedID == "" && time.Now().Before(deadline) {
		listResp, err := http.Get(ts.URL + "/v0/jobs")
		if err != nil {
			t.Fatal(err)
		}
		var entries []proto.JobListEntry
		json.NewDecoder(listResp.Body).Decode(&entries)
		listResp.Body.Close()
		for _, e := range entries {
			if e.State == proto.StateQueued {
				queuedID = e.ID
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if queuedID == "" {
		t.Fatalf("client job never queued; stderr: %s", errb.String())
	}
	time.Sleep(350 * time.Millisecond)

	forceKill(t, ts.URL, blocker)
	select {
	case code := <-done:
		if code != 5 {
			t.Fatalf("queued client run exit = %d, want 5; stderr: %s", code, errb.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("queued client run never completed")
	}
	if out.String() != "queued payload" {
		t.Fatalf("queued run output = %q", out.String())
	}
	if !bytes.Contains(errb.Bytes(), []byte("queued on the runner")) {
		t.Fatalf("client did not report queueing: %s", errb.String())
	}
}

func TestInfoReportsOccupancy(t *testing.T) {
	_, ts := concurrencyDaemon(t, 1, 1)
	root := workspaceWith(t, nil)

	blocker := proto.NewULID()
	resp := rawSubmit(t, ts.URL, blocker, root, []string{"/bin/sleep", "30"})
	resp.Body.Close()
	waitState(t, ts.URL, blocker, proto.StateRunning)
	queued := proto.NewULID()
	resp = rawSubmit(t, ts.URL, queued, root, []string{"/bin/echo", "hi"})
	resp.Body.Close()
	waitState(t, ts.URL, queued, proto.StateQueued)

	infoResp, err := http.Get(ts.URL + "/v0/info")
	if err != nil {
		t.Fatal(err)
	}
	var info proto.Info
	json.NewDecoder(infoResp.Body).Decode(&info)
	infoResp.Body.Close()
	if !info.Busy || info.RunningJobs != 1 || info.QueuedJobs != 1 || info.MaxJobs != 1 || info.MaxQueued != 1 {
		t.Fatalf("occupancy = %+v, want busy 1/1 running 1/1 queued", info)
	}

	forceKill(t, ts.URL, blocker)
	waitTerminal(t, ts.URL, blocker)
	waitTerminal(t, ts.URL, queued)
}
