package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 4 && os.Args[1] == "_automatic-apply" {
		if err := RunAutomaticApplyWorker(os.Args[2], os.Args[3]); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	stateHome, err := os.MkdirTemp("", "errand-client-test-state-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("XDG_STATE_HOME", stateHome); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(stateHome)
	os.Exit(code)
}

func testInterruptNotifications() interruptNotifications {
	return newInterruptNotifications(func() {}, func() {})
}

func TestSignalExitUsesSignalNumber(t *testing.T) {
	code := exitCode(proto.JobStatus{Result: &proto.Result{
		Signal: "segmentation fault", ChangesOK: true, CleanupOK: true, LogsComplete: true,
	}}, &bytes.Buffer{}, "peer/job")
	if code != 139 {
		t.Fatalf("SIGSEGV exit = %d, want 139", code)
	}
}

func TestControlEndpointsReturnDecodedResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/jobs/job-1":
			json.NewEncoder(w).Encode(proto.JobStatus{ID: "job-1", State: proto.StateRunning})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/storage":
			json.NewEncoder(w).Encode(proto.StorageStats{Jobs: proto.StorageCategory{Items: 2, Bytes: 58}})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/cache/gc":
			json.NewEncoder(w).Encode(proto.CacheGCResult{RemovedBlobs: 2, FreedBytes: 17})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/jobs":
			json.NewEncoder(w).Encode([]proto.JobListEntry{{ID: "job-1", State: proto.StateRunning}})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/info":
			json.NewEncoder(w).Encode(proto.Info{Proto: proto.ProtoVersion, Version: "test"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	status, err := getStatus(server.URL, "job-1")
	if err != nil || status.ID != "job-1" || status.State != proto.StateRunning {
		t.Fatalf("getStatus() = %+v, %v", status, err)
	}
	storage, err := StorageStats(server.URL)
	if err != nil || storage.Jobs.Items != 2 || storage.Jobs.Bytes != 58 {
		t.Fatalf("StorageStats() = %+v, %v", storage, err)
	}
	gc, err := CacheGC(server.URL)
	if err != nil || gc.RemovedBlobs != 2 || gc.FreedBytes != 17 {
		t.Fatalf("CacheGC() = %+v, %v", gc, err)
	}
	jobs, err := List(server.URL)
	if err != nil || len(jobs) != 1 || jobs[0].ID != "job-1" {
		t.Fatalf("List() = %+v, %v", jobs, err)
	}
	info, err := Info(server.URL)
	if err != nil || info.Proto != proto.ProtoVersion || info.Version != "test" {
		t.Fatalf("Info() = %+v, %v", info, err)
	}
}

func TestOutputlessRunSendsCanonicalLimits(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v0/snapshot/diff":
			http.NotFound(w, r)
		case r.Method == http.MethodPut:
			mr, err := r.MultipartReader()
			if err != nil {
				t.Errorf("multipart request: %v", err)
				return
			}
			part, err := mr.NextPart()
			if err != nil || part.FormName() != "spec" {
				t.Errorf("spec part = %v, %v", part, err)
				return
			}
			var wire map[string]any
			if err := json.NewDecoder(part).Decode(&wire); err != nil {
				t.Errorf("decode spec: %v", err)
				return
			}
			limits, _ := wire["limits"].(map[string]any)
			if got, exists := limits["max_change_bytes"]; !exists || int64(got.(float64)) != proto.DefaultLimits().MaxChangeBytes {
				t.Errorf("max_change_bytes = %v, want %d", got, proto.DefaultLimits().MaxChangeBytes)
			}
			for {
				part, err := mr.NextPart()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Errorf("remaining multipart body: %v", err)
					return
				}
				_, _ = io.Copy(io.Discard, part)
			}
			writeJSON := func(value any) { _ = json.NewEncoder(w).Encode(value) }
			writeJSON(proto.JobStatus{State: proto.StateRunning})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/logs"):
			w.Header().Set("Content-Type", "text/event-stream")
			zero := 0
			status, _ := json.Marshal(proto.JobStatus{State: proto.StateExited, Result: &proto.Result{
				ExitCode: &zero, ChangesOK: true, CleanupOK: true, LogsComplete: true,
			}})
			fmt.Fprintf(w, "event: status\ndata: %s\n\n", status)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var stderr bytes.Buffer
	interrupts := make(chan os.Signal, 2)
	interruptsReleased := atomic.Bool{}
	if code := runWithDetachNotifications(RunOptions{
		PeerURL: server.URL, Root: root, Argv: []string{"/bin/true"},
		Stdout: io.Discard, Stderr: &stderr,
	}, interrupts, newInterruptNotifications(
		func() { interruptsReleased.Store(true) }, func() {},
	), nil); code != 0 {
		t.Fatalf("output-less run exit = %d; stderr: %s", code, stderr.String())
	}
	if !interruptsReleased.Load() {
		t.Fatal("terminal run retained remote SIGINT ownership")
	}
}

func TestAdmissionBookkeepingFailureDoesNotAbandonAdmittedJob(t *testing.T) {
	previousConfirm := confirmAutomaticApplyAdmission
	previousLauncher := launchAutomaticApplyWorker
	t.Cleanup(func() {
		confirmAutomaticApplyAdmission = previousConfirm
		launchAutomaticApplyWorker = previousLauncher
	})
	confirmAutomaticApplyAdmission = func(string, string) error {
		return errors.New("state write unavailable")
	}
	launchAutomaticApplyWorker = func(string, string) error { return nil }

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var logRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v0/snapshot/diff":
			http.NotFound(w, r)
		case r.Method == http.MethodPut:
			_, _ = io.Copy(io.Discard, r.Body)
			_ = json.NewEncoder(w).Encode(proto.JobStatus{State: proto.StateRunning})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/logs"):
			logRequests.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			zero := 0
			status, _ := json.Marshal(proto.JobStatus{State: proto.StateExited, Result: &proto.Result{
				ExitCode: &zero, ChangesOK: true, CleanupOK: true, LogsComplete: true,
			}})
			fmt.Fprintf(w, "event: status\ndata: %s\n\n", status)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stderr bytes.Buffer
	code := runWithDetachNotifications(RunOptions{
		PeerURL: server.URL, Root: root, Argv: []string{"/bin/true"},
		ApplyOnSuccess: true, Stdout: io.Discard, Stderr: &stderr,
	}, make(chan os.Signal, 2), testInterruptNotifications(), nil)
	if code != 0 {
		t.Fatalf("run exit = %d, want remote success; stderr: %s", code, stderr.String())
	}
	if logRequests.Load() != 1 {
		t.Fatalf("log requests = %d, want admitted job to remain attached", logRequests.Load())
	}
	if !strings.Contains(stderr.String(), "recording automatic apply admission") {
		t.Fatalf("admission bookkeeping warning = %q", stderr.String())
	}
}

func TestAttachedApplyStartsCompletionWorkerAtAdmission(t *testing.T) {
	previousLauncher := launchAutomaticApplyWorker
	t.Cleanup(func() { launchAutomaticApplyWorker = previousLauncher })
	workerStarted := make(chan string, 1)
	launchAutomaticApplyWorker = func(peerURL, jobID string) error {
		workerStarted <- peerURL + "/" + jobID
		return nil
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v0/snapshot/diff":
			http.NotFound(w, r)
		case r.Method == http.MethodPut:
			_, _ = io.Copy(io.Discard, r.Body)
			_ = json.NewEncoder(w).Encode(proto.JobStatus{State: proto.StateRunning})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/logs"):
			select {
			case <-workerStarted:
			default:
				t.Error("log following began before automatic apply had a completion worker")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			zero := 0
			status, _ := json.Marshal(proto.JobStatus{State: proto.StateExited, Result: &proto.Result{
				ExitCode: &zero, ChangesOK: true, CleanupOK: true, LogsComplete: true,
			}})
			fmt.Fprintf(w, "event: status\ndata: %s\n\n", status)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stderr bytes.Buffer
	code := runWithDetachNotifications(RunOptions{
		PeerURL: server.URL, Root: root, Argv: []string{"/bin/true"},
		ApplyOnSuccess: true, Stdout: io.Discard, Stderr: &stderr,
	}, make(chan os.Signal, 2), testInterruptNotifications(), nil)
	if code != 0 {
		t.Fatalf("run exit = %d; stderr: %s", code, stderr.String())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCacheGCUsesMaintenanceDeadline(t *testing.T) {
	oldHTTP := maintenanceHTTP
	t.Cleanup(func() { maintenanceHTTP = oldHTTP })

	var remaining time.Duration
	maintenanceHTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		if !ok {
			t.Fatal("cache GC request has no deadline")
		}
		remaining = time.Until(deadline)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"removed_blobs":1,"freed_bytes":2}`)),
			Request:    req,
		}, nil
	})}

	result, err := CacheGC("http://runner")
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedBlobs != 1 || result.FreedBytes != 2 {
		t.Fatalf("CacheGC() = %+v", result)
	}
	if remaining <= controlRequestTimeout {
		t.Fatalf("cache GC deadline = %v, want longer than control timeout %v", remaining, controlRequestTimeout)
	}
	if maintenanceTransport.ResponseHeaderTimeout != maintenanceTimeout {
		t.Fatalf("maintenance response-header timeout = %v, want %v", maintenanceTransport.ResponseHeaderTimeout, maintenanceTimeout)
	}
}

func TestStorageStatsUsesStorageDeadline(t *testing.T) {
	oldHTTP := maintenanceHTTP
	t.Cleanup(func() { maintenanceHTTP = oldHTTP })

	var remaining time.Duration
	maintenanceHTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		if !ok {
			t.Fatal("storage stats request has no deadline")
		}
		remaining = time.Until(deadline)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"jobs":{"items":2,"bytes":58}}`)),
			Request:    req,
		}, nil
	})}

	stats, err := StorageStats("http://runner")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Jobs.Items != 2 || stats.Jobs.Bytes != 58 {
		t.Fatalf("StorageStats() = %+v", stats)
	}
	if remaining <= controlRequestTimeout || remaining > storageRequestTimeout {
		t.Fatalf("storage stats deadline = %v, want more than %v and at most %v",
			remaining, controlRequestTimeout, storageRequestTimeout)
	}
}

func TestSignaledExitReportsTransactionFailures(t *testing.T) {
	var stderr bytes.Buffer
	code := exitCode(proto.JobStatus{Result: &proto.Result{
		Signal: "killed", SignalNum: 9, ChangesOK: true,
		CleanupOK: false, LogsComplete: false, TransactionError: "scope cleanup failed",
	}}, &stderr, "peer/job")
	if code != 137 {
		t.Fatalf("SIGKILL exit = %d, want 137", code)
	}
	want := "errand: remote process killed by killed (cleanup failed) (logs truncated) (transaction error: scope cleanup failed)\n"
	if got := stderr.String(); got != want {
		t.Fatalf("signaled transaction report = %q, want %q", got, want)
	}
}

func TestExitDiagnosticsReportAllTransactionFailures(t *testing.T) {
	zero, seven := 0, 7
	for name, tc := range map[string]struct {
		status proto.JobStatus
		want   string
		code   int
	}{
		"successful process with incomplete changes": {
			status: proto.JobStatus{Result: &proto.Result{
				ExitCode: &zero, CleanupOK: true, LogsComplete: true,
			}},
			want: "errand: transaction incomplete (remote_exit=0, workspace changes incomplete)\n",
			code: ExitTransaction,
		},
		"failed process with secondary transaction failures": {
			status: proto.JobStatus{Result: &proto.Result{
				ExitCode: &seven, ChangesOK: true, CleanupOK: false, LogsComplete: false,
				LimitExceeded: "runtime", TransactionError: "persisting result failed",
			}},
			want: "errand: transaction incomplete (remote_exit=7, cleanup failed, limit exceeded: runtime, logs truncated, persisting result failed)\n",
			code: 7,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := exitCode(tc.status, &stderr, "peer/job"); got != tc.code {
				t.Fatalf("exit code = %d, want %d", got, tc.code)
			}
			if got := stderr.String(); got != tc.want {
				t.Fatalf("transaction report = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInterruptIsForwardedBeforeSubmitResponse(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	admitted := make(chan struct{})
	signaled := make(chan struct{})
	var controlAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				t.Errorf("reading submit body: %v", err)
				return
			}
			close(admitted)
			select {
			case <-signaled:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(proto.JobStatus{State: proto.StateRunning})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/signal"):
			switch controlAttempts.Add(1) {
			case 1:
				http.NotFound(w, r) // request raced admission
				return
			case 2:
				http.Error(w, "job is still staging", http.StatusConflict)
				return
			}
			close(signaled)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/logs"):
			w.Header().Set("Content-Type", "text/event-stream")
			status := proto.JobStatus{State: proto.StateKilled, Result: &proto.Result{
				Signal: "interrupt", SignalNum: 2, ChangesOK: true, CleanupOK: true, LogsComplete: true,
			}}
			payload, _ := json.Marshal(status)
			fmt.Fprintf(w, "event: status\ndata: %s\n\n", payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	interrupts := make(chan os.Signal, 2)
	done := make(chan int, 1)
	go func() {
		done <- runWithDetachNotifications(RunOptions{
			PeerURL: server.URL, Root: root, Argv: []string{"/bin/true"},
			Stdout: io.Discard, Stderr: io.Discard,
		}, interrupts, testInterruptNotifications(), nil)
	}()
	select {
	case <-admitted:
	case <-time.After(2 * time.Second):
		t.Fatal("submission did not reach the remote admission boundary")
	}
	interrupts <- os.Interrupt
	select {
	case code := <-done:
		if code != 130 {
			t.Fatalf("interrupted run exit = %d, want 130", code)
		}
		if got := controlAttempts.Load(); got != 3 {
			t.Fatalf("signal control attempts = %d, want 3", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SIGINT was not forwarded while the submit response was pending")
	}
}

func TestDetachedInterruptWaitsForForwardingAndDoesNotReportSuccess(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	admitted := make(chan struct{})
	firstControl := make(chan struct{})
	allowSubmitResponse := make(chan struct{})
	var controlAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				t.Errorf("reading submit body: %v", err)
				return
			}
			close(admitted)
			<-allowSubmitResponse
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(proto.JobStatus{State: proto.StateRunning})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/signal"):
			if controlAttempts.Add(1) == 1 {
				close(firstControl)
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	interrupts := make(chan os.Signal, 2)
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runWithDetachNotifications(RunOptions{
			PeerURL: server.URL, Root: root, Argv: []string{"/bin/true"},
			Detach: true, Stdout: &stdout, Stderr: &stderr,
		}, interrupts, testInterruptNotifications(), nil)
	}()
	select {
	case <-admitted:
	case <-time.After(2 * time.Second):
		t.Fatal("submission did not reach the remote admission boundary")
	}
	interrupts <- os.Interrupt
	select {
	case <-firstControl:
	case <-time.After(2 * time.Second):
		t.Fatal("SIGINT was not forwarded before the submit response")
	}
	close(allowSubmitResponse)
	select {
	case code := <-done:
		if code != 130 {
			t.Fatalf("detached interrupted run exit = %d, want 130; stderr: %s", code, stderr.String())
		}
		if got := strings.TrimSpace(stdout.String()); !strings.Contains(got, "/") {
			t.Fatalf("detached interrupted stdout = %q, want recoverable handle", got)
		}
		if got := controlAttempts.Load(); got != 2 {
			t.Fatalf("signal control attempts = %d, want 2", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("detached run did not wait for the admitted signal retry")
	}
}

func TestDetachDrainsInterruptDeliveredWhileResetting(t *testing.T) {
	interrupts := make(chan os.Signal, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resumed := atomic.Bool{}
	controller := startAdmittedJobController(
		ctx, interrupts,
		newInterruptTarget(
			"http://invalid", proto.NewULID(), "peer/job", func(string, ...any) {},
			newInterruptNotifications(
				func() { interrupts <- os.Interrupt },
				func() { resumed.Store(true) },
			),
		),
	)
	if controller.detach(ctx) {
		t.Fatal("detach reported success after SIGINT was delivered while notification was stopping")
	}
	select {
	case <-controller.remote:
	case <-time.After(time.Second):
		t.Fatal("interrupt delivered during reset was not handed to remote control")
	}
	if !resumed.Load() {
		t.Fatal("SIGINT notification was not resumed for force-kill escalation")
	}
}

func TestInteractiveDetachRequestedBeforeAdmissionSkipsLogFollowing(t *testing.T) {
	previousLauncher := launchAutomaticApplyWorker
	t.Cleanup(func() { launchAutomaticApplyWorker = previousLauncher })
	workerStarted := make(chan string, 1)
	launchAutomaticApplyWorker = func(peerURL, jobID string) error {
		workerStarted <- peerURL + "/" + jobID
		return nil
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var logRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			_, _ = io.Copy(io.Discard, r.Body)
			json.NewEncoder(w).Encode(proto.JobStatus{State: proto.StateRunning})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/logs"):
			logRequests.Add(1)
			http.Error(w, "unexpected log follow", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	detach := make(chan struct{})
	close(detach)
	var stderr bytes.Buffer
	code := runWithDetachNotifications(RunOptions{
		PeerURL: server.URL, Root: root, Argv: []string{"/bin/true"},
		ApplyOnSuccess: true, Stdout: io.Discard, Stderr: &stderr,
	}, make(chan os.Signal, 2), testInterruptNotifications(), detach)
	if code != 0 {
		t.Fatalf("pre-admission interactive detach exit = %d; stderr: %s", code, stderr.String())
	}
	if logRequests.Load() != 0 {
		t.Fatalf("pre-admission detach followed logs %d times", logRequests.Load())
	}
	select {
	case target := <-workerStarted:
		if !strings.HasPrefix(target, server.URL+"/") {
			t.Fatalf("automatic apply worker target = %q", target)
		}
	default:
		t.Fatal("detachment did not hand automatic apply to a completion worker")
	}
	for _, want := range []string{"detached", "reattach with", server.URL + "/"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("pre-admission detach report %q does not contain %q", stderr.String(), want)
		}
	}
}

func TestDetachedApplyReportsWorkerLaunchFailure(t *testing.T) {
	previousLauncher := launchAutomaticApplyWorker
	t.Cleanup(func() { launchAutomaticApplyWorker = previousLauncher })
	launchAutomaticApplyWorker = func(string, string) error { return errors.New("worker unavailable") }

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/snapshot/diff"):
			http.NotFound(w, r)
		case r.Method == http.MethodPut:
			_, _ = io.Copy(io.Discard, r.Body)
			_ = json.NewEncoder(w).Encode(proto.JobStatus{State: proto.StateRunning})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := runWithDetachNotifications(RunOptions{
		PeerURL: server.URL, Root: root, Argv: []string{"/bin/true"},
		Detach: true, ApplyOnSuccess: true, Stdout: &stdout, Stderr: &stderr,
	}, make(chan os.Signal, 2), testInterruptNotifications(), nil)
	if code != ExitTransaction {
		t.Fatalf("detached apply launch failure exit = %d, want %d; stderr: %s", code, ExitTransaction, stderr.String())
	}
	if !proto.ValidULID(strings.TrimSpace(strings.TrimPrefix(stdout.String(), server.URL+"/"))) {
		t.Fatalf("detached apply did not preserve handle on stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "automatic workspace change application could not continue") {
		t.Fatalf("worker launch failure diagnostic = %q", stderr.String())
	}
	jobID := strings.TrimSpace(strings.TrimPrefix(stdout.String(), server.URL+"/"))
	state, err := loadLocalChangeState(server.URL, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if state.AutomaticApply != automaticApplyPending || !strings.Contains(state.AutomaticApplyErr, "worker unavailable") {
		t.Fatalf("worker launch failure state = %+v, want resumable pending state", state)
	}
}

func TestAutomaticApplyCompletionUsesSingleOwner(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: peerURL, Root: root, ManifestRoot: (proto.Manifest{}).RootHash(),
		SubmissionStarted: true, AdmissionConfirmed: true, ApplyOnSuccess: true,
		AutomaticApply: automaticApplyPending,
	}); err != nil {
		t.Fatal(err)
	}
	key := localChangeKey(peerURL, jobID)
	unlock, err := acquireLocalChangeLock(localAutomaticApplyLockName(key))
	if err != nil {
		t.Fatal(err)
	}

	zero := 0
	done := make(chan error, 1)
	go func() {
		_, err := applyTerminalAutomatically(peerURL, jobID, proto.JobStatus{
			State:  proto.StateExited,
			Result: &proto.Result{ExitCode: &zero, ChangesOK: true, CleanupOK: true, LogsComplete: true},
		})
		done <- err
	}()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("automatic apply bypassed its ownership lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("automatic apply did not continue after ownership was released")
	}
}

func TestAutomaticApplyWorkerProcessUsesPersistedPolicy(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	jobID := proto.NewULID()
	zero := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v0/jobs/"+jobID {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(proto.JobStatus{ID: jobID, State: proto.StateExited, Result: &proto.Result{
			ExitCode: &zero, ChangesOK: true, CleanupOK: true, LogsComplete: true,
		}})
	}))
	defer server.Close()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: server.URL, Root: root, ManifestRoot: (proto.Manifest{}).RootHash(),
		SubmissionStarted: true, AdmissionConfirmed: true, ApplyOnSuccess: true,
		AutomaticApply: automaticApplyPending,
	}); err != nil {
		t.Fatal(err)
	}
	if err := startAutomaticApplyWorkerProcess(server.URL, jobID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, err := loadLocalChangeState(server.URL, jobID)
		if err == nil && state.AutomaticApply == automaticApplyNoChanges {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	state, err := loadLocalChangeState(server.URL, jobID)
	t.Fatalf("detached worker did not settle persisted policy: state=%+v error=%v", state, err)
}

func TestAutomaticApplyWorkerSettlesSuccessfulJobWithoutChanges(t *testing.T) {
	root := t.TempDir()
	jobID := proto.NewULID()
	zero := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v0/jobs/"+jobID {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(proto.JobStatus{ID: jobID, State: proto.StateExited, Result: &proto.Result{
			ExitCode: &zero, ChangesOK: true, CleanupOK: true, LogsComplete: true,
		}})
	}))
	defer server.Close()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: server.URL, Root: root, ManifestRoot: (proto.Manifest{}).RootHash(),
		SubmissionStarted: true, AdmissionConfirmed: true, ApplyOnSuccess: true,
		AutomaticApply: automaticApplyPending,
	}); err != nil {
		t.Fatal(err)
	}

	if err := runAutomaticApplyWorkerContext(context.Background(), server.URL, jobID, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	state, err := loadLocalChangeState(server.URL, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Terminal || state.AutomaticApply != automaticApplyNoChanges {
		t.Fatalf("automatic apply state = terminal %t, status %q", state.Terminal, state.AutomaticApply)
	}
}

func TestAutomaticApplyWorkerLeaseDeduplicatesPollers(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	jobID := proto.NewULID()
	zero := 0
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releasePoll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releasePoll()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v0/jobs/"+jobID {
			http.NotFound(w, r)
			return
		}
		if requests.Add(1) == 1 {
			close(entered)
		}
		<-release
		_ = json.NewEncoder(w).Encode(proto.JobStatus{ID: jobID, State: proto.StateExited, Result: &proto.Result{
			ExitCode: &zero, ChangesOK: true, CleanupOK: true, LogsComplete: true,
		}})
	}))
	defer server.Close()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: server.URL, Root: root, ManifestRoot: (proto.Manifest{}).RootHash(),
		SubmissionStarted: true, AdmissionConfirmed: true, ApplyOnSuccess: true,
		AutomaticApply: automaticApplyPending,
	}); err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- RunAutomaticApplyWorker(server.URL, jobID) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first automatic apply worker did not begin polling")
	}
	secondDone := make(chan error, 1)
	go func() { secondDone <- RunAutomaticApplyWorker(server.URL, jobID) }()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("duplicate automatic apply worker began polling")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("status requests before releasing worker = %d, want 1", got)
	}
	releasePoll()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owning automatic apply worker did not finish")
	}
	stateRoot, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	leasePath := filepath.Join(
		stateRoot, "locks",
		localAutomaticApplyWorkerLockName(localChangeKey(server.URL, jobID))+".lock",
	)
	if _, err := os.Lstat(leasePath); !os.IsNotExist(err) {
		t.Fatalf("completed automatic apply worker retained lease file: %v", err)
	}
}

func TestAutomaticApplyCompletionLocksAreBounded(t *testing.T) {
	names := make(map[string]bool)
	for range localChangeLockStripes * 4 {
		jobID := proto.NewULID()
		key := localChangeKey("http://runner.test", jobID)
		names[localAutomaticApplyLockName(key)] = true
	}
	if len(names) > localChangeLockStripes {
		t.Fatalf("automatic apply created %d completion lock names, want at most %d", len(names), localChangeLockStripes)
	}
}

func TestAutomaticApplyKeepsTransientFetchFailurePending(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	jobID := proto.NewULID()
	zero := 0
	summary := &proto.ChangeSummary{
		Paths: []string{"artifact"}, PathCount: 1, BundleRoot: strings.Repeat("a", 64), Bytes: 1,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/jobs/" + jobID:
			_ = json.NewEncoder(w).Encode(proto.JobDetails{JobStatus: proto.JobStatus{
				ID: jobID, State: proto.StateExited, Result: &proto.Result{
					ExitCode: &zero, ChangesOK: true, CleanupOK: true, LogsComplete: true, Changes: summary,
				},
			}})
		case "/v0/jobs/" + jobID + "/changes":
			http.Error(w, "try again", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: server.URL, Root: root, ManifestRoot: testManifestRoot,
		SubmissionStarted: true, AdmissionConfirmed: true, ApplyOnSuccess: true,
		AutomaticApply: automaticApplyPending,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := applyTerminalAutomatically(server.URL, jobID, proto.JobStatus{
		ID: jobID, State: proto.StateExited, Result: &proto.Result{
			ExitCode: &zero, ChangesOK: true, CleanupOK: true, LogsComplete: true, Changes: summary,
		},
	})
	if err == nil {
		t.Fatal("transient fetch failure unexpectedly succeeded")
	}
	state, loadErr := loadLocalChangeState(server.URL, jobID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if state.AutomaticApply != automaticApplyPending || !strings.Contains(state.AutomaticApplyErr, "503") {
		t.Fatalf("transient automatic apply state = %+v, want resumable pending", state)
	}
}

func TestLateAutomaticApplyFailureCannotOverwriteSuccess(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: peerURL, Root: t.TempDir(), ManifestRoot: testManifestRoot,
		SubmissionStarted: true, AdmissionConfirmed: true, ApplyOnSuccess: true,
		AutomaticApply: automaticApplyApplied, AutomaticApplyDir: "/staged/success",
	}); err != nil {
		t.Fatal(err)
	}
	if err := recordAutomaticApply(peerURL, jobID, automaticApplyOutcome{
		state: automaticApplyFailed, err: "late worker failure",
	}); err != nil {
		t.Fatal(err)
	}
	state, err := loadLocalChangeState(peerURL, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if state.AutomaticApply != automaticApplyApplied || state.AutomaticApplyDir != "/staged/success" || state.AutomaticApplyErr != "" {
		t.Fatalf("late failure overwrote terminal success: %+v", state)
	}
}

func TestAutomaticApplyTreatsMissingConfirmedJobAsPermanent(t *testing.T) {
	err := &controlHTTPError{statusCode: http.StatusNotFound, err: errors.New("missing")}
	if automaticApplyStatusErrorIsPermanent(err, false) {
		t.Fatal("unconfirmed missing job was treated as permanent")
	}
	if !automaticApplyStatusErrorIsPermanent(err, true) {
		t.Fatal("confirmed missing job was retried forever")
	}
}

func TestResumeAutomaticAppliesRestartsOnlyUnfinishedPolicies(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	previousLauncher := launchAutomaticApplyWorker
	t.Cleanup(func() { launchAutomaticApplyWorker = previousLauncher })
	started := make(chan string, 3)
	launchAutomaticApplyWorker = func(peerURL, jobID string) error {
		started <- peerURL + "/" + jobID
		return nil
	}

	workspace := t.TempDir()
	peerURL := "http://runner.test"
	pendingID := proto.NewULID()
	activeID := proto.NewULID()
	for _, state := range []localChangeState{
		{
			JobID: pendingID, PeerURL: peerURL, Root: workspace, ManifestRoot: (proto.Manifest{}).RootHash(),
			SubmissionStarted: true, ApplyOnSuccess: true, AutomaticApply: automaticApplyPending,
		},
		{
			JobID: activeID, PeerURL: peerURL, Root: workspace, ManifestRoot: (proto.Manifest{}).RootHash(),
			SubmissionStarted: true, ApplyOnSuccess: true, AutomaticApply: automaticApplyPending,
		},
		{
			JobID: proto.NewULID(), PeerURL: peerURL, Root: workspace, ManifestRoot: (proto.Manifest{}).RootHash(),
			SubmissionStarted: true, ApplyOnSuccess: true, AutomaticApply: automaticApplyApplied,
		},
		{
			JobID: proto.NewULID(), PeerURL: peerURL, Root: workspace, ManifestRoot: (proto.Manifest{}).RootHash(),
			SubmissionStarted: true,
		},
	} {
		if err := saveLocalChangeState(state); err != nil {
			t.Fatal(err)
		}
	}
	unlock, acquired, err := tryAcquireLocalChangeLease(
		localAutomaticApplyWorkerLockName(localChangeKey(peerURL, activeID)),
	)
	if err != nil || !acquired {
		t.Fatalf("acquiring active worker lease = %t, %v", acquired, err)
	}
	defer unlock()

	if err := ResumeAutomaticApplies(); err != nil {
		t.Fatal(err)
	}
	resumed := map[string]bool{}
	for len(started) != 0 {
		resumed[<-started] = true
	}
	if !resumed[peerURL+"/"+pendingID] {
		t.Fatal("unfinished automatic apply was not resumed")
	}
	if resumed[peerURL+"/"+activeID] {
		t.Fatal("active automatic apply worker was redundantly resumed")
	}
	if len(resumed) != 1 {
		t.Fatalf("unexpected automatic applies resumed: %v", resumed)
	}
}

func TestAutomaticApplyWorkerEnvironmentOmitsUnrelatedSecrets(t *testing.T) {
	t.Setenv("ERRAND_TEST_SECRET", "do-not-inherit")
	for _, entry := range automaticApplyWorkerEnvironment() {
		if strings.HasPrefix(entry, "ERRAND_TEST_SECRET=") {
			t.Fatalf("automatic apply worker inherited unrelated secret: %q", entry)
		}
	}
}

func TestInteractiveDetachCancelsOnlyTheLocalLogFollow(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	logStarted := make(chan struct{})
	logCanceled := make(chan struct{})
	var controlRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			_, _ = io.Copy(io.Discard, r.Body)
			json.NewEncoder(w).Encode(proto.JobStatus{State: proto.StateRunning})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/logs"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			close(logStarted)
			<-r.Context().Done()
			close(logCanceled)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/snapshot/diff"):
			http.NotFound(w, r)
		case r.Method == http.MethodPost:
			controlRequests.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	detach := make(chan struct{})
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runWithDetachNotifications(RunOptions{
			PeerURL: server.URL, Root: root, Argv: []string{"/bin/sleep", "30"},
			Stdout: io.Discard, Stderr: &stderr,
		}, make(chan os.Signal, 2), testInterruptNotifications(), detach)
	}()
	select {
	case <-logStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("log follow did not start")
	}
	close(detach)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("interactive detach exit = %d; stderr: %s", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("interactive detach did not return promptly")
	}
	select {
	case <-logCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("interactive detach did not cancel the local log request")
	}
	if controlRequests.Load() != 0 {
		t.Fatalf("interactive detach sent %d remote control requests", controlRequests.Load())
	}
	if !strings.Contains(stderr.String(), "detached") || !strings.Contains(stderr.String(), "reattach with") {
		t.Fatalf("interactive detach report = %q", stderr.String())
	}
}

type gatedWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *gatedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

func TestStreamDetachWaitsForFollowerShutdown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 1\nevent: log\ndata: {\"seq\":1,\"stream\":\"stdout\",\"data_b64\":\"eA==\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	writer := &gatedWriter{started: make(chan struct{}), release: make(chan struct{})}
	detach := make(chan struct{})
	done := make(chan bool, 1)
	go func() {
		_, _, detached := streamUntilDetach(
			RunOptions{PeerURL: server.URL, Stdout: writer, Stderr: io.Discard},
			"job", proto.JobStatus{ID: "job", State: proto.StateRunning}, detach,
		)
		done <- detached
	}()
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("log follower never reached the output writer")
	}
	close(detach)
	select {
	case <-done:
		t.Fatal("detach returned while the log follower was still writing")
	case <-time.After(50 * time.Millisecond):
	}
	close(writer.release)
	select {
	case detached := <-done:
		if !detached {
			t.Fatal("stream completion won after detach had linearized")
		}
	case <-time.After(time.Second):
		t.Fatal("detach did not finish after the follower stopped")
	}
}

func TestAttachCanDetachWithoutControllingTheRemoteJob(t *testing.T) {
	jobID := proto.NewULID()
	logStarted := make(chan struct{})
	var controlRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/jobs/"+jobID:
			json.NewEncoder(w).Encode(proto.JobStatus{ID: jobID, State: proto.StateRunning})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/logs"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			close(logStarted)
			<-r.Context().Done()
		case r.Method == http.MethodPost:
			controlRequests.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	detach := make(chan struct{})
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- attachWithDetachNotifications(AttachOptions{
			PeerURL: server.URL, JobID: jobID, Stdout: io.Discard, Stderr: &stderr,
		}, make(chan os.Signal, 2), testInterruptNotifications(), detach)
	}()
	select {
	case <-logStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("attach log follow did not start")
	}
	close(detach)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("attach detach exit = %d; stderr: %s", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("attach detach did not return promptly")
	}
	if controlRequests.Load() != 0 {
		t.Fatalf("attach detach sent %d remote control requests", controlRequests.Load())
	}
}

func TestAttachDoesNotFetchTerminalWorkspaceChanges(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	jobID := proto.NewULID()
	var changeRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/jobs/"+jobID:
			_ = json.NewEncoder(w).Encode(proto.JobStatus{ID: jobID, State: proto.StateRunning})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/logs"):
			w.Header().Set("Content-Type", "text/event-stream")
			zero := 0
			status, _ := json.Marshal(proto.JobStatus{ID: jobID, State: proto.StateExited, Result: &proto.Result{
				ExitCode: &zero, ChangesOK: true, CleanupOK: true, LogsComplete: true,
				Changes: &proto.ChangeSummary{PathCount: 1, Paths: []string{"artifact"}},
			}})
			fmt.Fprintf(w, "event: status\ndata: %s\n\n", status)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/changes"):
			changeRequests.Add(1)
			http.Error(w, "attach must not fetch changes", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: server.URL, Root: t.TempDir(), ManifestRoot: (proto.Manifest{}).RootHash(),
	}); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	code := attachWithDetachNotifications(AttachOptions{
		PeerURL: server.URL, JobID: jobID, Stdout: io.Discard, Stderr: &stderr,
	}, make(chan os.Signal, 2), testInterruptNotifications(), nil)
	if code != 0 {
		t.Fatalf("attach exit = %d; stderr: %s", code, stderr.String())
	}
	if got := changeRequests.Load(); got != 0 {
		t.Fatalf("attach made %d workspace change requests", got)
	}
	if strings.Contains(stderr.String(), "workspace changes staged") {
		t.Fatalf("attach staged workspace changes: %s", stderr.String())
	}
	state, err := loadLocalChangeState(server.URL, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Terminal {
		t.Fatal("attach did not settle the local terminal state")
	}
}

func TestDetachOnEOFIsTTYOnly(t *testing.T) {
	if ch := detachOnEOFContext(context.Background(), strings.NewReader(""), false); ch != nil {
		t.Fatal("non-TTY EOF enabled interactive detachment")
	}
	r, w := io.Pipe()
	ch := detachOnEOFContext(context.Background(), r, true)
	if ch == nil {
		t.Fatal("TTY EOF did not enable interactive detachment")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("TTY EOF did not request detachment")
	}
}

func TestEOFWatcherDoesNotReadWhileBackgrounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{}, 1)
	backgroundChecked := make(chan struct{})
	var backgroundOnce sync.Once
	foreground := atomic.Bool{}
	readCalls := atomic.Int32{}
	detach := watchTerminalEOF(ctx, terminalEOFOps{
		foreground: func() bool {
			isForeground := foreground.Load()
			if !isForeground {
				backgroundOnce.Do(func() { close(backgroundChecked) })
			}
			return isForeground
		},
		waitReadable: func(time.Duration) (bool, error) {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-ready:
				return true, nil
			}
		},
		read: func([]byte) (int, error) {
			readCalls.Add(1)
			return 0, io.EOF
		},
	})

	select {
	case <-backgroundChecked:
	case <-time.After(time.Second):
		t.Fatal("EOF watcher did not inspect background terminal state")
	}
	if got := readCalls.Load(); got != 0 {
		t.Fatalf("background EOF watcher performed %d reads", got)
	}
	foreground.Store(true)
	ready <- struct{}{}
	select {
	case <-detach:
	case <-time.After(time.Second):
		t.Fatal("foreground EOF did not request detachment")
	}
}

func TestInterruptBeforeAdmissionDoesNotSubmit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected remote request", http.StatusInternalServerError)
	}))
	defer server.Close()

	interrupts := make(chan os.Signal, 2)
	interrupts <- os.Interrupt
	code := runWithDetachNotifications(RunOptions{
		PeerURL: server.URL, Root: root, Argv: []string{"/bin/true"},
		Stdout: io.Discard, Stderr: io.Discard,
	}, interrupts, testInterruptNotifications(), nil)
	if code != 130 {
		t.Fatalf("pre-admission interrupt exit = %d, want 130", code)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("pre-admission interrupt made %d remote requests, want 0", got)
	}
}

func TestInterruptCancelsSnapshotNegotiation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var puts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/snapshot/diff":
			close(started)
			select {
			case <-r.Context().Done():
			case <-release:
				http.Error(w, "released", http.StatusServiceUnavailable)
			}
		case r.Method == http.MethodPut:
			puts.Add(1)
			http.Error(w, "unexpected submit", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	interrupts := make(chan os.Signal, 2)
	done := make(chan int, 1)
	go func() {
		done <- runWithDetachNotifications(RunOptions{
			PeerURL: server.URL, Root: root, Argv: []string{"/bin/true"},
			Stdout: io.Discard, Stderr: io.Discard,
		}, interrupts, testInterruptNotifications(), nil)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("snapshot negotiation did not start")
	}
	interrupts <- os.Interrupt
	select {
	case code := <-done:
		if code != 130 {
			t.Fatalf("negotiation interrupt exit = %d, want 130", code)
		}
	case <-time.After(time.Second):
		t.Fatal("interrupt did not cancel snapshot negotiation promptly")
	}
	if got := puts.Load(); got != 0 {
		t.Fatalf("interrupt during negotiation submitted %d jobs", got)
	}
}

func TestQueuedInterruptAtAdmissionStaysLocal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interrupts := make(chan os.Signal, 1)
	interrupts <- os.Interrupt
	controller := admitJobController(
		ctx, interrupts,
		newInterruptTarget(
			"http://invalid", proto.NewULID(), "peer/job", func(string, ...any) {},
			testInterruptNotifications(),
		),
	)
	if controller != nil {
		t.Fatal("queued pre-admission interrupt started remote control")
	}
}

func TestSecondInterruptRestoresDefaultBeforeForceKillRetry(t *testing.T) {
	killStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/signal"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/kill"):
			close(killStarted)
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interrupts := make(chan os.Signal, 2)
	reset := make(chan struct{})
	var resetOnce sync.Once
	controller := startAdmittedJobController(
		ctx, interrupts,
		newInterruptTarget(
			server.URL, proto.NewULID(), "peer/job", func(string, ...any) {},
			newInterruptNotifications(
				func() { resetOnce.Do(func() { close(reset) }) },
				func() {},
			),
		),
	)
	interrupts <- os.Interrupt
	interrupts <- os.Interrupt
	select {
	case <-killStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("force-kill request did not start")
	}
	select {
	case <-reset:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("default SIGINT handling was not restored while force-kill was retrying")
	}
	cancel()
	select {
	case <-controller.done:
	case <-time.After(2 * time.Second):
		t.Fatal("interrupt forwarder did not stop after cancellation")
	}
}

func TestJobControlRetriesTransientServerFailure(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := retryJobControl(ctx, server.URL, nil, true); err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("control attempts = %d, want 2", got)
	}
}

func TestIdleReadCloserTimesOut(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	reader := &idleReadCloser{ReadCloser: r, timeout: 10 * time.Millisecond}
	defer reader.Close()

	_, err := reader.Read(make([]byte, 1))
	if err == nil || !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("idle read error = %v, want timeout", err)
	}
}

func TestDirectTransportDoesNotUseProxyEnvironment(t *testing.T) {
	if directTransport.Proxy != nil {
		t.Fatal("direct errand transport must not consult HTTP proxy settings")
	}
	if directHTTP.CheckRedirect == nil {
		t.Fatal("direct errand client must not follow redirects to another endpoint")
	}
}

func TestPeerLabelPreservesRawURL(t *testing.T) {
	if got := peerLabel("", "http://runner:9000/"); got != "http://runner:9000" {
		t.Fatalf("raw URL peer label = %q, want port-preserving URL", got)
	}
	if got := peerLabel("cabal", "http://runner:9000"); got != "cabal" {
		t.Fatalf("configured peer label = %q, want alias", got)
	}
}

func TestControlJSONGetCancelsAStalledResponseBody(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var status proto.JobStatus
	done := make(chan error, 1)
	go func() {
		done <- getJSONContext(ctx, server.URL, 1<<20, "job lookup", &status)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("server never started the response body")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("stalled control response error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stalled control response ignored its request deadline")
	}
}

func TestAmbiguousResultReportsTransactionExplanation(t *testing.T) {
	var stderr bytes.Buffer
	code := exitCode(proto.JobStatus{State: proto.StateAmbiguous, Result: &proto.Result{
		State: proto.StateAmbiguous, StartError: "exec format error", ChangesOK: false, CleanupOK: false, LogsComplete: false,
		TransactionError: "execution state unknown; not replayed",
	}}, &stderr, "cabal/job")
	if code != ExitTransaction {
		t.Fatalf("ambiguous result exit = %d, want %d", code, ExitTransaction)
	}
	want := "errand: transaction incomplete (cabal/job, state=ambiguous, start error: exec format error, workspace changes incomplete, cleanup failed, logs truncated, execution state unknown; not replayed)\n"
	if got := stderr.String(); got != want {
		t.Fatalf("ambiguous transaction report = %q, want %q", got, want)
	}
}

func TestAmbiguousStatePreservesRecordedNonzeroProcessOutcome(t *testing.T) {
	codeSeven := 7
	for name, tc := range map[string]struct {
		result *proto.Result
		want   int
	}{
		"exit": {
			result: &proto.Result{State: proto.StateAmbiguous, ExitCode: &codeSeven, ChangesOK: true, CleanupOK: true,
				LogsComplete: true, TransactionError: "persisting result failed"},
			want: 7,
		},
		"signal": {
			result: &proto.Result{State: proto.StateAmbiguous, Signal: "killed", SignalNum: 9, ChangesOK: true,
				CleanupOK: true, LogsComplete: true, TransactionError: "persisting result failed"},
			want: 137,
		},
		"start": {
			result: &proto.Result{State: proto.StateAmbiguous, StartError: "exec format error", ChangesOK: true,
				CleanupOK: true, LogsComplete: true, TransactionError: "rollback failed"},
			want: ExitTransaction,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := exitCode(proto.JobStatus{
				State: proto.StateAmbiguous, Result: tc.result,
			}, &stderr, "cabal/job")
			if code != tc.want {
				t.Fatalf("ambiguous %s exit = %d, want %d", name, code, tc.want)
			}
			for _, want := range []string{"ambiguous", tc.result.TransactionError} {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("ambiguous %s report %q does not contain %q", name, stderr.String(), want)
				}
			}
		})
	}
}

func TestAmbiguousSuccessfulExitRemainsTransactionFailure(t *testing.T) {
	zero := 0
	code := exitCode(proto.JobStatus{State: proto.StateAmbiguous, Result: &proto.Result{
		State: proto.StateAmbiguous, ExitCode: &zero, ChangesOK: true, CleanupOK: true,
		LogsComplete: true, TransactionError: "persisting result failed",
	}}, io.Discard, "cabal/job")
	if code != ExitTransaction {
		t.Fatalf("ambiguous successful exit = %d, want %d", code, ExitTransaction)
	}
}

func TestTerminalJobLogReplayFailureIsTransactionFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "log unavailable", http.StatusBadRequest)
	}))
	defer server.Close()
	code := 0
	_, err := streamContext(context.Background(), RunOptions{PeerURL: server.URL}, "job", proto.JobStatus{
		ID: "job", State: proto.StateExited,
		Result: &proto.Result{ExitCode: &code, ChangesOK: true, CleanupOK: true, LogsComplete: true},
	})
	if err == nil {
		t.Fatal("terminal log replay failure was discarded")
	}
}

func TestTerminalJobRetriesTransientLogReplay(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: status\ndata: {\"id\":\"job\",\"state\":\"exited\",\"result\":{}}\n\n")
	}))
	defer server.Close()
	code := 0
	final, err := streamContext(context.Background(), RunOptions{PeerURL: server.URL}, "job", proto.JobStatus{
		ID: "job", State: proto.StateExited,
		Result: &proto.Result{ExitCode: &code, ChangesOK: true, CleanupOK: true, LogsComplete: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || final.State != proto.StateExited {
		t.Fatalf("terminal replay requests = %d, final = %+v", requests.Load(), final)
	}
}

func TestPermanentLogHTTPFailureIsNotRetried(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()
	_, err := streamContext(context.Background(), RunOptions{PeerURL: server.URL}, "job", proto.JobStatus{ID: "job", State: proto.StateRunning})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("permanent log response error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("permanent log response was attempted %d times", requests.Load())
	}
}

func TestNotImplementedLogHTTPFailureIsNotRetried(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unsupported", http.StatusNotImplemented)
	}))
	defer server.Close()
	_, err := streamContext(context.Background(), RunOptions{PeerURL: server.URL}, "job", proto.JobStatus{ID: "job", State: proto.StateRunning})
	if err == nil || !strings.Contains(err.Error(), "501") {
		t.Fatalf("not-implemented log response error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("not-implemented log response was attempted %d times", requests.Load())
	}
}

func TestRetryableRemoteLogErrorResumes(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: error\ndata: {\"message\":\"temporary read failure\",\"retryable\":true}\n\n")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: status\ndata: {\"id\":\"job\",\"state\":\"exited\",\"result\":{}}\n\n")
	}))
	defer server.Close()
	final, err := streamContext(context.Background(), RunOptions{PeerURL: server.URL}, "job", proto.JobStatus{ID: "job", State: proto.StateRunning})
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || final.State != proto.StateExited {
		t.Fatalf("retryable replay requests = %d, final = %+v", requests.Load(), final)
	}
}

func TestMalformedLogFrameCannotAdvanceResumeCursor(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "id: 1\nevent: log\ndata: {\"seq\":1,\"stream\":\"stdout\",\"data_b64\":\"%%%\"}\n\n")
	}))
	defer server.Close()
	_, err := streamContext(context.Background(), RunOptions{PeerURL: server.URL, Stdout: io.Discard, Stderr: io.Discard}, "job", proto.JobStatus{
		ID: "job", State: proto.StateRunning,
	})
	if err == nil {
		t.Fatal("malformed log frame was treated as a retryable disconnect")
	}
	if requests.Load() != 1 {
		t.Fatalf("malformed persisted frame was retried %d times", requests.Load())
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return max(0, len(p)-1), nil }

func TestShortOutputWriteFailsTransaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "id: 1\nevent: log\ndata: {\"seq\":1,\"stream\":\"stdout\",\"data_b64\":\"eA==\"}\n\n")
	}))
	defer server.Close()
	_, err := streamContext(context.Background(), RunOptions{PeerURL: server.URL, Stdout: shortWriter{}, Stderr: io.Discard}, "job", proto.JobStatus{
		ID: "job", State: proto.StateRunning,
	})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short output write error = %v, want io.ErrShortWrite", err)
	}
}

func TestSubmitRetriesSameJobIDAfterLostResponse(t *testing.T) {
	root := t.TempDir()
	manifest, err := snapshot.Build(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/bin/true"},
		ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits(),
	}
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if attempts.Add(1) == 1 {
			panic(http.ErrAbortHandler) // admission succeeded, response was lost
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(proto.JobStatus{ID: "same", State: proto.StateRunning})
	}))
	defer server.Close()

	_, _, err = submit(RunOptions{PeerURL: server.URL, Root: root}, "same", spec, manifest, shipPlan{})
	if err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("submit attempts = %d, want 2", got)
	}
}

func TestSubmitBusyReportsCapacityInsteadOfSingleJob(t *testing.T) {
	root := t.TempDir()
	manifest, err := snapshot.Build(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/bin/true"},
		ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits(),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{"error": "runner capacity is full"})
	}))
	defer server.Close()

	_, _, err = submitOnce(RunOptions{PeerURL: server.URL, Root: root}, "job", spec, manifest, shipPlan{})
	if err == nil || !strings.Contains(err.Error(), "capacity") || strings.Contains(err.Error(), "one job at a time") {
		t.Fatalf("busy diagnostic = %v", err)
	}
}

func TestStreamDeadlineRefreshesWhileQueued(t *testing.T) {
	now := time.Unix(100, 0)
	tracker := newStreamDeadlineTracker(now, proto.JobStatus{State: proto.StateQueued})
	original := tracker.deadline
	later := now.Add(time.Hour)
	tracker.observe(later, proto.JobStatus{State: proto.StateQueued})
	if !tracker.deadline.After(original) {
		t.Fatalf("queued deadline was not refreshed: original=%v refreshed=%v", original, tracker.deadline)
	}
	runningDeadline := tracker.deadline
	tracker.observe(later.Add(time.Minute), proto.JobStatus{State: proto.StateRunning})
	if tracker.phase != proto.StateRunning || !tracker.deadline.After(runningDeadline) {
		t.Fatalf("running phase did not start a fresh deadline: phase=%q deadline=%v", tracker.phase, tracker.deadline)
	}
}

func TestCacheMissGetsIndependentFullSubmitRetryBudget(t *testing.T) {
	root := t.TempDir()
	const content = "cache fallback payload"
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshot.Build(root, []string{"file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/bin/true"},
		ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits(),
	}
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch attempts.Add(1) {
		case 1, 2:
			panic(http.ErrAbortHandler)
		case 3:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "negotiated cache content was evicted",
				"code":  "snapshot_cache_miss",
			})
		case 4:
			if !bytes.Contains(body, []byte(content)) {
				http.Error(w, "full snapshot was not shipped", http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(proto.JobStatus{ID: "same", State: proto.StateRunning})
		default:
			http.Error(w, "unexpected retry", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	var stderr bytes.Buffer
	_, _, err = submit(RunOptions{PeerURL: server.URL, Root: root, Stderr: &stderr}, "same", spec, manifest, shipPlan{partial: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != 4 {
		t.Fatalf("submit attempts = %d, want 4", got)
	}
	if !strings.Contains(stderr.String(), "re-shipping the full snapshot") {
		t.Fatalf("fallback diagnostic = %q", stderr.String())
	}
}

func TestCacheFallbackPreservesPriorAdmissionUncertainty(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshot.Build(root, []string{"file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/bin/true"},
		ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits(),
	}
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch attempts.Add(1) {
		case 1:
			panic(http.ErrAbortHandler)
		case 2:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "negotiated cache content was evicted",
				"code":  proto.ErrorCodeSnapshotCacheMiss,
			})
		case 3:
			http.Error(w, "full snapshot rejected", http.StatusBadRequest)
		default:
			http.Error(w, "unexpected retry", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	_, uncertain, err := submit(
		RunOptions{PeerURL: server.URL, Root: root, Stderr: io.Discard},
		"same", spec, manifest, shipPlan{partial: true},
	)
	if err == nil {
		t.Fatal("submit unexpectedly succeeded")
	}
	if !uncertain {
		t.Fatal("cache fallback discarded uncertainty from the earlier transport failure")
	}
}
