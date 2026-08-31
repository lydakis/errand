package client

import (
	"bytes"
	"context"
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

	outputops "github.com/lydakis/errand/internal/outputs"
	"github.com/lydakis/errand/internal/proto"
)

func TestLocalOutputClientIDIsStable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	first, err := localOutputClientID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := localOutputClientID()
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !proto.ValidOutputClientID(first) {
		t.Fatalf("local output client IDs = %q and %q", first, second)
	}
}

func TestLocalOutputClientIDCleansInterruptedTemporaryFile(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := filepath.Join(stateHome, "errand")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, ".client-id-interrupted")
	if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	id, err := localOutputClientID()
	if err != nil {
		t.Fatal(err)
	}
	if !proto.ValidOutputClientID(id) {
		t.Fatalf("local output client ID = %q", id)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("interrupted client ID file survived: %v", err)
	}
}

func TestRecoveryIgnoresMalformedUnrelatedOutputState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateRoot, err := localOutputRoot()
	if err != nil {
		t.Fatal(err)
	}
	jobs := filepath.Join(stateRoot, "jobs")
	if err := os.MkdirAll(jobs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobs, strings.Repeat("a", 64)+".json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverWorkspaceApplications(t.TempDir()); err != nil {
		t.Fatalf("unrelated malformed state blocked recovery: %v", err)
	}
}

func TestRecoveryWithoutWorkspaceTransactionIgnoresGlobalStateDirectory(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	stateRoot, err := localOutputRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateRoot, "jobs"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverWorkspaceApplications(t.TempDir()); err != nil {
		t.Fatalf("recovery without a workspace transaction read global state: %v", err)
	}
}

func TestRecoveryFailsClosedWhenMalformedStateMayOwnWorkspaceTransaction(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateRoot, err := localOutputRoot()
	if err != nil {
		t.Fatal(err)
	}
	jobs := filepath.Join(stateRoot, "jobs")
	if err := os.MkdirAll(jobs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobs, strings.Repeat("a", 64)+".json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, outputops.NewApplyTransaction()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := recoverWorkspaceApplications(workspace); err == nil || !strings.Contains(err.Error(), "loading") {
		t.Fatalf("recovery with unowned transaction error = %v", err)
	}
}

func TestRecoveryFailsClosedWhenWorkspaceTransactionHasNoStateDirectory(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, outputops.NewApplyTransaction()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := recoverWorkspaceApplications(workspace); err == nil || !strings.Contains(err.Error(), "no matching local output state") {
		t.Fatalf("recovery without private state error = %v", err)
	}
}

func TestRecoveryFailsClosedWhenWorkspaceTransactionHasNoMatchingState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateRoot, err := localOutputRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateRoot, "jobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, outputops.NewApplyTransaction()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := recoverWorkspaceApplications(workspace); err == nil || !strings.Contains(err.Error(), "no matching local output state") {
		t.Fatalf("recovery without matching state error = %v", err)
	}
}

func TestLocalOutputStateRejectsDifferentPeer(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	jobID := proto.NewULID()
	if err := saveLocalOutputState(localOutputState{
		Version: localOutputStateVersion, JobID: jobID, PeerURL: "http://runner-a.test", Root: workspace,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLocalOutputState("http://runner-b.test", jobID); !os.IsNotExist(err) {
		t.Fatalf("different-peer lookup error = %v, want not exist", err)
	}
}

func TestDefiniteSubmitRejectionRemovesOutputState(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("baseline"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v0/snapshot/diff" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPut {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "runner capacity is full"})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	code := runWithDetachNotifications(RunOptions{
		PeerURL: server.URL, Root: root, Argv: []string{"/bin/true"},
		Outputs: []proto.OutputSpec{{Path: "artifact"}}, Stdout: io.Discard, Stderr: io.Discard,
	}, make(chan os.Signal), testInterruptNotifications(), nil)
	if code != ExitTransaction {
		t.Fatalf("rejected run exit = %d", code)
	}
	entries, err := os.ReadDir(filepath.Join(stateHome, "errand", "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("definitely rejected job retained output state: %v", entries)
	}
}

func TestSubmitRetryConflictRetainsOutputStateAndHandle(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("baseline"), 0o600); err != nil {
		t.Fatal(err)
	}
	var puts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v0/snapshot/diff" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPut {
			puts++
			if puts == 1 {
				panic(http.ErrAbortHandler)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "job id exists, but request identity cannot be verified after restart",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	var stderr bytes.Buffer
	code := runWithDetachNotifications(RunOptions{
		PeerURL: server.URL, Root: root, Argv: []string{"/bin/true"},
		Outputs: []proto.OutputSpec{{Path: "artifact"}}, Stdout: io.Discard, Stderr: &stderr,
	}, make(chan os.Signal), testInterruptNotifications(), nil)
	if code != ExitTransaction {
		t.Fatalf("ambiguous run exit = %d", code)
	}
	entries, err := os.ReadDir(filepath.Join(stateHome, "errand", "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("ambiguous submit retained %d output states, want 1", len(entries))
	}
	if !strings.Contains(stderr.String(), "the job may have been admitted; handle ") {
		t.Fatalf("ambiguous submit diagnostic = %q", stderr.String())
	}
}

func TestOutputDownloadsForDifferentJobsRunConcurrently(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := outputops.Collect(remote, jobDir, []proto.OutputSpec{{
		Path: "artifact", Collect: proto.OutputCollectAlways, Apply: proto.OutputApplyManual,
	}}, true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	jobA, jobB := proto.NewULID(), proto.NewULID()
	enteredA := make(chan struct{})
	releaseA := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseA) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/jobs/"+jobA+"/outputs" {
			close(enteredA)
			<-releaseA
		}
		mw := multipart.NewWriter(w)
		w.Header().Set("Content-Type", mw.FormDataContentType())
		metadata, _ := mw.CreateFormField("bundle")
		_ = json.NewEncoder(metadata).Encode(bundle)
		archivePart, _ := mw.CreateFormFile("archive", "archive.tar")
		archiveFile, openErr := outputops.OpenArchive(jobDir)
		if openErr != nil {
			t.Error(openErr)
			return
		}
		_, _ = io.Copy(archivePart, archiveFile)
		_ = archiveFile.Close()
		_ = mw.Close()
	}))
	defer func() {
		release()
		server.Close()
	}()
	for localOutputTransferLockName(localOutputKey(server.URL, jobA)) ==
		localOutputTransferLockName(localOutputKey(server.URL, jobB)) {
		jobB = proto.NewULID()
	}
	summary := proto.OutputSummary{Paths: bundle.Paths, ManifestRoot: bundle.Manifest.RootHash(), Bytes: bundle.Bytes}
	doneA := make(chan error, 1)
	go func() {
		_, _, err := downloadOutputBundle(server.URL, jobA, summary)
		doneA <- err
	}()
	select {
	case <-enteredA:
	case <-time.After(5 * time.Second):
		t.Fatal("first download did not reach the server")
	}
	doneB := make(chan error, 1)
	go func() {
		_, _, err := downloadOutputBundle(server.URL, jobB, summary)
		doneB <- err
	}()
	select {
	case err := <-doneB:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		release()
		<-doneA
		t.Fatal("an unrelated output download was serialized behind the first")
	}
	release()
	if err := <-doneA; err != nil {
		t.Fatal(err)
	}
}

func TestInterruptCancelsOutputBaselineLockWait(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("baseline"), 0o600); err != nil {
		t.Fatal(err)
	}

	locked := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- withWorkspaceOutputLock(root, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	sigCh := make(chan os.Signal, 1)
	done := make(chan int, 1)
	go func() {
		done <- runWithDetachNotifications(RunOptions{
			PeerURL: "http://runner.test", Root: root, Argv: []string{"/bin/true"},
			Outputs: []proto.OutputSpec{{Path: "artifact"}}, Stdout: io.Discard, Stderr: io.Discard,
		}, sigCh, testInterruptNotifications(), nil)
	}()
	time.Sleep(100 * time.Millisecond)
	sigCh <- os.Interrupt
	select {
	case code := <-done:
		if code != signalExit("interrupt", 2) {
			t.Fatalf("interrupted run exit = %d", code)
		}
	case <-time.After(2 * time.Second):
		close(release)
		<-holderDone
		t.Fatal("interrupt did not cancel output baseline lock wait")
	}
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceOutputLockContextCanBeCanceled(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	locked := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- withWorkspaceOutputLock(root, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := withWorkspaceOutputLockContext(ctx, root, func() error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("withWorkspaceOutputLockContext() error = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
}

func TestLocalOutputLockNamesAreBounded(t *testing.T) {
	names := map[string]bool{}
	for i := 0; i < 10_000; i++ {
		name := localOutputTransferLockName(fmt.Sprintf("job-%d", i))
		names[name] = true
		if strings.Contains(name, fmt.Sprintf("job-%d", i)) {
			t.Fatalf("lock name exposes unbounded key: %q", name)
		}
	}
	if len(names) > localOutputLockStripes {
		t.Fatalf("created %d distinct lock names, want at most %d", len(names), localOutputLockStripes)
	}
}

func TestInterruptedSnapshotNegotiationRemovesUnsubmittedOutputState(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("baseline"), 0o600); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v0/snapshot/diff" {
			http.NotFound(w, r)
			return
		}
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	sigCh := make(chan os.Signal, 1)
	done := make(chan int, 1)
	go func() {
		done <- runWithDetachNotifications(RunOptions{
			PeerURL: server.URL, Root: root, Argv: []string{"/bin/true"},
			Outputs: []proto.OutputSpec{{Path: "artifact"}}, Stdout: io.Discard, Stderr: io.Discard,
		}, sigCh, testInterruptNotifications(), nil)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot negotiation did not start")
	}
	sigCh <- os.Interrupt
	select {
	case code := <-done:
		if code != signalExit("interrupt", 2) {
			t.Fatalf("interrupted run exit = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupted run did not return")
	}
	jobs := filepath.Join(stateHome, "errand", "jobs")
	entries, err := os.ReadDir(jobs)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unsubmitted output state survived: %v", entries)
	}
}

func TestDownloadOutputBundleHasNoTotalRequestDeadline(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := outputops.Collect(remote, jobDir, []proto.OutputSpec{{
		Path: "artifact", Collect: proto.OutputCollectAlways, Apply: proto.OutputApplyManual,
	}}, true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var payload bytes.Buffer
	mw := multipart.NewWriter(&payload)
	metadata, err := mw.CreateFormField("bundle")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(metadata).Encode(bundle); err != nil {
		t.Fatal(err)
	}
	archivePart, err := mw.CreateFormFile("archive", "archive.tar")
	if err != nil {
		t.Fatal(err)
	}
	archiveFile, err := outputops.OpenArchive(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(archivePart, archiveFile); err != nil {
		archiveFile.Close()
		t.Fatal(err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	oldHTTP := maintenanceHTTP
	t.Cleanup(func() { maintenanceHTTP = oldHTTP })
	maintenanceHTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if _, ok := req.Context().Deadline(); ok {
			t.Error("output download has a total request deadline")
		}
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK",
			Header: http.Header{"Content-Type": []string{mw.FormDataContentType()}},
			Body:   io.NopCloser(bytes.NewReader(payload.Bytes())), Request: req,
		}, nil
	})}
	summary := proto.OutputSummary{Paths: bundle.Paths, ManifestRoot: bundle.Manifest.RootHash(), Bytes: bundle.Bytes}
	staged, _, err := downloadOutputBundle("http://runner.test", proto.NewULID(), summary)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(staged, "workspace", "artifact")); err != nil || string(got) != "new" {
		t.Fatalf("downloaded artifact = %q, %v", got, err)
	}
}

func TestDownloadOutputBundleRefreshesCachedStaging(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := outputops.Collect(remote, jobDir, []proto.OutputSpec{{
		Path: "artifact", Collect: proto.OutputCollectAlways, Apply: proto.OutputApplyManual,
	}}, true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		mw := multipart.NewWriter(w)
		w.Header().Set("Content-Type", mw.FormDataContentType())
		metadata, _ := mw.CreateFormField("bundle")
		_ = json.NewEncoder(metadata).Encode(bundle)
		archivePart, _ := mw.CreateFormFile("archive", "archive.tar")
		archiveFile, openErr := outputops.OpenArchive(jobDir)
		if openErr != nil {
			t.Error(openErr)
			return
		}
		_, _ = io.Copy(archivePart, archiveFile)
		_ = archiveFile.Close()
		_ = mw.Close()
	}))
	defer server.Close()
	summary := proto.OutputSummary{Paths: bundle.Paths, ManifestRoot: bundle.Manifest.RootHash(), Bytes: bundle.Bytes}
	jobID := proto.NewULID()
	staged, _, err := downloadOutputBundle(server.URL, jobID, summary)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(staged, old, old); err != nil {
		t.Fatal(err)
	}
	refreshedAfter := time.Now().Add(-time.Second)
	if _, _, err := downloadOutputBundle(server.URL, jobID, summary); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(staged)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Before(refreshedAfter) {
		t.Fatalf("cached staging mtime = %v, want at least %v", info.ModTime(), refreshedAfter)
	}
	if requests != 1 {
		t.Fatalf("cached output made %d requests, want 1", requests)
	}
}

func TestFetchOutputsReplacesIncompleteStagingAndBindsReceipt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := outputops.Collect(remote, jobDir, []proto.OutputSpec{{
		Path: "artifact", Collect: proto.OutputCollectAlways, Apply: proto.OutputApplyManual,
	}}, true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	jobID := proto.NewULID()
	summary := &proto.OutputSummary{
		Paths: bundle.Paths, ManifestRoot: bundle.Manifest.RootHash(), Bytes: bundle.Bytes,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/jobs/" + jobID:
			json.NewEncoder(w).Encode(proto.JobStatus{Result: &proto.Result{OutputsOK: true, Outputs: summary}})
		case "/v0/jobs/" + jobID + "/outputs":
			mw := multipart.NewWriter(w)
			w.Header().Set("Content-Type", mw.FormDataContentType())
			metadata, _ := mw.CreateFormField("bundle")
			_ = json.NewEncoder(metadata).Encode(bundle)
			archivePart, _ := mw.CreateFormFile("archive", "archive.tar")
			archiveFile, openErr := outputops.OpenArchive(jobDir)
			if openErr != nil {
				t.Error(openErr)
				return
			}
			_, _ = io.Copy(archivePart, archiveFile)
			_ = archiveFile.Close()
			_ = mw.Close()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	root, err := localOutputRoot()
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "downloads", localOutputKey(server.URL, jobID))
	if err := os.MkdirAll(filepath.Join(dest, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "workspace", "stale"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	staged, err := FetchOutputs(OutputFetchOptions{PeerURL: server.URL, JobID: jobID})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(staged, "workspace", "artifact"))
	if err != nil || string(got) != "new" {
		t.Fatalf("repaired staging = %q, %v", got, err)
	}
}

func TestInitializeOutputStateRecoversPendingApplicationWithoutNewOutputs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := outputops.Collect(remote, jobDir, []proto.OutputSpec{{
		Path: "artifact", Collect: proto.OutputCollectAlways, Apply: proto.OutputApplyManual,
	}}, true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "artifact"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselines, err := outputops.CaptureBaselines(local, []proto.OutputSpec{{Path: "artifact"}})
	if err != nil {
		t.Fatal(err)
	}
	staged := t.TempDir()
	archiveFile, err := outputops.OpenArchive(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := outputops.Extract(archiveFile, staged, bundle, 1<<20); err != nil {
		archiveFile.Close()
		t.Fatal(err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatal(err)
	}
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	owner := localOutputKey(peerURL, jobID)
	transaction := outputops.NewApplyTransaction()
	state := localOutputState{
		Version: localOutputStateVersion, JobID: jobID, PeerURL: peerURL, Root: local,
		Outputs: []proto.OutputSpec{{Path: "artifact"}}, Baselines: baselines, Pending: transaction,
	}
	if err := saveLocalOutputState(state); err != nil {
		t.Fatal(err)
	}
	if _, err := outputops.Apply(staged, local, bundle, baselines, nil, owner, transaction); err != nil {
		t.Fatal(err)
	}
	if err := initializeOutputState(context.Background(), &RunOptions{Root: local}, proto.NewULID()); err != nil {
		t.Fatal(err)
	}
	recovered, err := loadLocalOutputState(peerURL, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Pending != "" || recovered.Applied["artifact"] != bundle.Manifest.RootHash() {
		t.Fatalf("recovered state = %+v", recovered)
	}
	if _, err := os.Lstat(filepath.Join(local, transaction)); !os.IsNotExist(err) {
		t.Fatalf("committed transaction survived state recovery: %v", err)
	}
}

func TestRecoverWorkspaceApplicationsContextRefusesCanceledRecovery(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	transaction := outputops.NewApplyTransaction()
	if err := saveLocalOutputState(localOutputState{
		Version: localOutputStateVersion, JobID: jobID, PeerURL: peerURL,
		Root: root, Pending: transaction,
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := recoverWorkspaceApplicationsContext(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("recoverWorkspaceApplicationsContext() error = %v, want context.Canceled", err)
	}
	state, err := loadLocalOutputState(peerURL, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Pending != transaction {
		t.Fatalf("canceled recovery changed pending transaction to %q", state.Pending)
	}
}

func TestWorkspaceOutputLockSerializesCallers(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withWorkspaceOutputLock(root, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	second := make(chan struct{})
	go func() {
		_ = withWorkspaceOutputLock(root, func() error {
			close(second)
			return nil
		})
	}()
	select {
	case <-second:
		t.Fatal("second caller entered while the workspace lock was held")
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-second
}

func TestOutputSummaryMatchesAllReceiptFields(t *testing.T) {
	bundle := proto.OutputBundle{V: outputops.BundleVersion, Paths: []string{"artifact"}, Bytes: 3}
	summary := proto.OutputSummary{Paths: []string{"artifact"}, ManifestRoot: bundle.Manifest.RootHash(), Bytes: 3}
	if !outputSummaryMatches(bundle, summary) {
		t.Fatal("matching summary was rejected")
	}
	summary.Bytes++
	if outputSummaryMatches(bundle, summary) {
		t.Fatal("byte-mismatched summary was accepted")
	}
}

func TestOutputGCRetainsPendingTransactionsAndRemovesOldCompletedState(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	workspace := t.TempDir()
	peerURL := "http://runner.test"
	old := time.Now().Add(-48 * time.Hour)

	completedID := proto.NewULID()
	completed := localOutputState{
		Version: localOutputStateVersion, JobID: completedID, PeerURL: peerURL, Root: workspace, Terminal: true,
	}
	if err := saveLocalOutputState(completed); err != nil {
		t.Fatal(err)
	}
	pendingID := proto.NewULID()
	pending := localOutputState{
		Version: localOutputStateVersion, JobID: pendingID, PeerURL: peerURL, Root: workspace,
		Pending: outputops.NewApplyTransaction(),
	}
	if err := saveLocalOutputState(pending); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, pending.Pending), 0o700); err != nil {
		t.Fatal(err)
	}
	stalePendingID := proto.NewULID()
	stalePending := localOutputState{
		Version: localOutputStateVersion, JobID: stalePendingID, PeerURL: peerURL, Root: workspace,
		Pending: outputops.NewApplyTransaction(),
	}
	if err := saveLocalOutputState(stalePending); err != nil {
		t.Fatal(err)
	}
	root, err := localOutputRoot()
	if err != nil {
		t.Fatal(err)
	}
	completedPath := filepath.Join(root, "jobs", localOutputKey(peerURL, completedID)+".json")
	pendingPath := filepath.Join(root, "jobs", localOutputKey(peerURL, pendingID)+".json")
	stalePendingPath := filepath.Join(root, "jobs", localOutputKey(peerURL, stalePendingID)+".json")
	if err := os.Chtimes(completedPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pendingPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stalePendingPath, old, old); err != nil {
		t.Fatal(err)
	}
	activeID := proto.NewULID()
	active := localOutputState{
		Version: localOutputStateVersion, JobID: activeID, PeerURL: peerURL, Root: workspace,
		SubmissionStarted: true,
	}
	if err := saveLocalOutputState(active); err != nil {
		t.Fatal(err)
	}
	activePath := filepath.Join(root, "jobs", localOutputKey(peerURL, activeID)+".json")
	if err := os.Chtimes(activePath, old, old); err != nil {
		t.Fatal(err)
	}
	result, err := OutputGC(24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 2 || result.Protected != 2 || result.Failed != 0 {
		t.Fatalf("OutputGC() = %+v", result)
	}
	if _, err := os.Lstat(completedPath); !os.IsNotExist(err) {
		t.Fatalf("completed state survived GC: %v", err)
	}
	if _, err := os.Lstat(pendingPath); err != nil {
		t.Fatalf("pending state was collected: %v", err)
	}
	if _, err := os.Lstat(stalePendingPath); !os.IsNotExist(err) {
		t.Fatalf("journal-free pending state survived GC: %v", err)
	}
	if _, err := os.Lstat(activePath); err != nil {
		t.Fatalf("active-job baseline was collected: %v", err)
	}
}

func TestSyncExistingLocalDirectoryPropagatesSyncFailure(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := syncExistingLocalDirectory(filepath.Join(blocker, "jobs"))
	if err == nil {
		t.Fatal("syncExistingLocalDirectory() unexpectedly succeeded")
	}
	if err := syncExistingLocalDirectory(filepath.Join(root, "missing")); err != nil {
		t.Fatalf("missing directory: %v", err)
	}
}

func TestOutputGCRetainsPendingStateWhileWorkspaceIsUnavailable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	state := localOutputState{
		Version: localOutputStateVersion, JobID: jobID, PeerURL: peerURL, Root: workspace,
		Pending: outputops.NewApplyTransaction(),
	}
	if err := saveLocalOutputState(state); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, state.Pending), 0o700); err != nil {
		t.Fatal(err)
	}
	offline := workspace + "-offline"
	if err := os.Rename(workspace, offline); err != nil {
		t.Fatal(err)
	}
	root, err := localOutputRoot()
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "jobs", localOutputKey(peerURL, jobID)+".json")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(statePath, old, old); err != nil {
		t.Fatal(err)
	}

	result, err := OutputGC(24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 0 || result.Protected != 1 || result.Failed != 0 {
		t.Fatalf("OutputGC() = %+v", result)
	}
	if _, err := os.Lstat(statePath); err != nil {
		t.Fatalf("pending state was collected while its workspace was unavailable: %v", err)
	}
}

func TestOutputGCRemovesOldUnsubmittedState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	if err := saveLocalOutputState(localOutputState{
		Version: localOutputStateVersion, JobID: jobID, PeerURL: peerURL, Root: workspace,
	}); err != nil {
		t.Fatal(err)
	}
	root, err := localOutputRoot()
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "jobs", localOutputKey(peerURL, jobID)+".json")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(statePath, old, old); err != nil {
		t.Fatal(err)
	}
	result, err := OutputGC(24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 1 || result.Protected != 0 {
		t.Fatalf("OutputGC() = %+v", result)
	}
	if _, err := os.Lstat(statePath); !os.IsNotExist(err) {
		t.Fatalf("unsubmitted state survived GC: %v", err)
	}
}

func TestOutputGCRemovesAbandonedSubmittedState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	if err := saveLocalOutputState(localOutputState{
		Version: localOutputStateVersion, JobID: jobID, PeerURL: peerURL, Root: workspace,
		SubmissionStarted: true,
	}); err != nil {
		t.Fatal(err)
	}
	root, err := localOutputRoot()
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "jobs", localOutputKey(peerURL, jobID)+".json")
	old := time.Now().Add(-unresolvedOutputStateProtection - time.Hour)
	if err := os.Chtimes(statePath, old, old); err != nil {
		t.Fatal(err)
	}
	result, err := OutputGC(24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 1 || result.Protected != 0 {
		t.Fatalf("OutputGC() = %+v", result)
	}
	if _, err := os.Lstat(statePath); !os.IsNotExist(err) {
		t.Fatalf("abandoned submitted state survived GC: %v", err)
	}
}

func TestOutputGCSkipsActiveTransfer(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	if err := saveLocalOutputState(localOutputState{
		Version: localOutputStateVersion, JobID: jobID, PeerURL: peerURL, Root: workspace, Terminal: true,
	}); err != nil {
		t.Fatal(err)
	}
	root, err := localOutputRoot()
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "jobs", localOutputKey(peerURL, jobID)+".json")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(statePath, old, old); err != nil {
		t.Fatal(err)
	}
	unlock, err := acquireLocalOutputLock(localOutputTransferLockName(localOutputKey(peerURL, jobID)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := OutputGC(24*time.Hour, false)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if result.Protected != 1 || result.Removed != 0 {
		t.Fatalf("OutputGC() = %+v", result)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("active transfer state was collected: %v", err)
	}
}

func TestOutputGCRevalidatesDownloadedOutputsAfterScan(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	if err := saveLocalOutputState(localOutputState{
		Version: localOutputStateVersion, JobID: jobID, PeerURL: peerURL, Root: workspace, Terminal: true,
	}); err != nil {
		t.Fatal(err)
	}
	root, err := localOutputRoot()
	if err != nil {
		t.Fatal(err)
	}
	key := localOutputKey(peerURL, jobID)
	downloadPath := filepath.Join(root, "downloads", key)
	if err := os.MkdirAll(downloadPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(downloadPath, "bundle.json"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	statePath := filepath.Join(root, "jobs", key+".json")
	for _, path := range []string{statePath, downloadPath} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	candidates := map[string]*localOutputCandidate{}
	if err := collectOutputGCCandidates(filepath.Join(root, "jobs"), filepath.Join(root, "downloads"), candidates); err != nil {
		t.Fatal(err)
	}
	candidate := candidates[key]
	if candidate == nil {
		t.Fatal("output GC did not discover staged outputs")
	}
	if err := os.Chtimes(downloadPath, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	removed, eligible, protected, err := collectLocalOutputCandidate(candidate, time.Now().Add(-24*time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	if removed || eligible || protected {
		t.Fatalf("refreshed candidate = removed %t, eligible %t, protected %t", removed, eligible, protected)
	}
	for _, path := range []string{statePath, downloadPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("refreshed output path %s was collected: %v", path, err)
		}
	}
}

func TestReconcileCollectedJobOutputsCollectsUnobservedTerminalJob(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	jobID := proto.NewULID()
	var collectedCalls, acknowledgementCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/jobs/collected" {
			if r.URL.Path == "/v0/jobs/collected/ack" && r.Method == http.MethodPost {
				acknowledgementCalls++
				var request proto.CollectedJobsAck
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if !proto.ValidOutputClientID(request.ClientID) || len(request.JobIDs) != 1 || request.JobIDs[0] != jobID {
					t.Errorf("collection acknowledgement = %+v", request)
				}
				json.NewEncoder(w).Encode(proto.CollectedJobsAckResult{Acknowledged: 1})
				return
			}
			http.NotFound(w, r)
			return
		}
		if !proto.ValidOutputClientID(r.URL.Query().Get("client_id")) {
			t.Errorf("invalid collection client ID %q", r.URL.Query().Get("client_id"))
		}
		collectedCalls++
		if r.URL.Query().Get("cursor") == "" {
			json.NewEncoder(w).Encode(proto.CollectedJobsPage{JobIDs: []string{jobID}, NextCursor: jobID})
			return
		}
		if r.URL.Query().Get("cursor") != jobID {
			t.Errorf("collection cursor = %q, want %q", r.URL.Query().Get("cursor"), jobID)
		}
		json.NewEncoder(w).Encode(proto.CollectedJobsPage{})
	}))
	defer server.Close()
	if err := saveLocalOutputState(localOutputState{
		Version: localOutputStateVersion, JobID: jobID, PeerURL: server.URL, Root: workspace,
	}); err != nil {
		t.Fatal(err)
	}
	root, err := localOutputRoot()
	if err != nil {
		t.Fatal(err)
	}
	key := localOutputKey(server.URL, jobID)
	downloadPath := filepath.Join(root, "downloads", key)
	if err := os.MkdirAll(downloadPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(downloadPath, "bundle.json"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "jobs", key+".json")
	old := time.Now().Add(-48 * time.Hour)
	for _, path := range []string{statePath, downloadPath} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	if err := ReconcileCollectedJobOutputs(server.URL); err != nil {
		t.Fatal(err)
	}
	if collectedCalls != 2 {
		t.Fatalf("collected jobs calls = %d, want 2", collectedCalls)
	}
	if acknowledgementCalls != 1 {
		t.Fatalf("collection acknowledgement calls = %d, want 1", acknowledgementCalls)
	}
	state, err := loadLocalOutputState(server.URL, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Terminal {
		t.Fatal("remote removal did not settle the local output state")
	}
	result, err := OutputGC(24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Selected != 1 || result.Removed != 1 || result.Protected != 0 || result.Failed != 0 {
		t.Fatalf("OutputGC() after reconciliation = %+v", result)
	}
	for _, path := range []string{statePath, downloadPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("removed remote job retained local output path %s: %v", path, err)
		}
	}
}

func TestMarkLocalOutputTerminalPersistsAfterWorkspaceRemoval(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	if err := saveLocalOutputState(localOutputState{
		Version: localOutputStateVersion, JobID: jobID, PeerURL: peerURL, Root: workspace,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(workspace); err != nil {
		t.Fatal(err)
	}
	if err := markLocalOutputTerminal(peerURL, jobID); err != nil {
		t.Fatal(err)
	}
	state, err := loadLocalOutputState(peerURL, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Terminal {
		t.Fatal("terminal observation was not persisted")
	}
}

func TestRepeatApplyRefusesDestinationChangedAfterFirstApply(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("remote"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := outputops.Collect(remote, jobDir, []proto.OutputSpec{{
		Path: "artifact", Collect: proto.OutputCollectAlways, Apply: proto.OutputApplyManual,
	}}, true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "artifact"), []byte("baseline"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselines, err := outputops.CaptureBaselines(local, []proto.OutputSpec{{Path: "artifact"}})
	if err != nil {
		t.Fatal(err)
	}
	staged := t.TempDir()
	stagedWorkspace := filepath.Join(staged, "workspace")
	if err := os.Mkdir(stagedWorkspace, 0o700); err != nil {
		t.Fatal(err)
	}
	archiveFile, err := outputops.OpenArchive(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := outputops.Extract(archiveFile, stagedWorkspace, bundle, 1<<20); err != nil {
		archiveFile.Close()
		t.Fatal(err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatal(err)
	}
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	if err := saveLocalOutputState(localOutputState{
		Version: localOutputStateVersion, JobID: jobID, PeerURL: peerURL, Root: local,
		Outputs: []proto.OutputSpec{{Path: "artifact"}}, Baselines: baselines,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := applyOutputBundle(peerURL, jobID, t.TempDir(), staged, bundle, nil); err == nil || !strings.Contains(err.Error(), "within the workspace") {
		t.Fatalf("apply from unrelated workspace error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(local, "artifact"))
	if err != nil || string(got) != "baseline" {
		t.Fatalf("unrelated apply changed destination = %q, %v", got, err)
	}
	callerDir := filepath.Join(local, "src", "package")
	if err := os.MkdirAll(callerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := applyOutputBundle(peerURL, jobID, callerDir, staged, bundle, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "artifact"), []byte("user edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := applyOutputBundle(peerURL, jobID, local, staged, bundle, nil); err == nil || !strings.Contains(err.Error(), "changed after it was applied") {
		t.Fatalf("repeat apply error = %v", err)
	}
	got, err = os.ReadFile(filepath.Join(local, "artifact"))
	if err != nil || string(got) != "user edit" {
		t.Fatalf("repeat apply changed destination = %q, %v", got, err)
	}
}

func TestApplyRefusesWorkspaceReplacedAtSamePath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("remote"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := outputops.Collect(remote, jobDir, []proto.OutputSpec{{
		Path: "artifact", Collect: proto.OutputCollectAlways, Apply: proto.OutputApplyManual,
	}}, true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	container := t.TempDir()
	local := filepath.Join(container, "workspace")
	if err := os.Mkdir(local, 0o700); err != nil {
		t.Fatal(err)
	}
	baselines, err := outputops.CaptureBaselines(local, []proto.OutputSpec{{Path: "artifact"}})
	if err != nil {
		t.Fatal(err)
	}
	staged := t.TempDir()
	stagedWorkspace := filepath.Join(staged, "workspace")
	if err := os.Mkdir(stagedWorkspace, 0o700); err != nil {
		t.Fatal(err)
	}
	archiveFile, err := outputops.OpenArchive(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := outputops.Extract(archiveFile, stagedWorkspace, bundle, 1<<20); err != nil {
		archiveFile.Close()
		t.Fatal(err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatal(err)
	}
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	if err := saveLocalOutputState(localOutputState{
		Version: localOutputStateVersion, JobID: jobID, PeerURL: peerURL, Root: local,
		Outputs: []proto.OutputSpec{{Path: "artifact"}}, Baselines: baselines,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(local, filepath.Join(container, "original-workspace")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(local, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := applyOutputBundle(peerURL, jobID, local, staged, bundle, nil); err == nil || !strings.Contains(err.Error(), "is not the workspace") {
		t.Fatalf("apply to replacement workspace error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(local, "artifact")); !os.IsNotExist(err) {
		t.Fatalf("output reached replacement workspace: %v", err)
	}
}
