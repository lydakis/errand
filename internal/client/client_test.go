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
	"sync/atomic"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestSignalExitUsesSignalNumber(t *testing.T) {
	code := exitCode(proto.JobStatus{Result: &proto.Result{
		Signal: "segmentation fault", OutputsOK: true, CleanupOK: true, LogsComplete: true,
	}}, &bytes.Buffer{}, "peer/job")
	if code != 139 {
		t.Fatalf("SIGSEGV exit = %d, want 139", code)
	}
}

func TestSignaledExitReportsTransactionFailures(t *testing.T) {
	var stderr bytes.Buffer
	code := exitCode(proto.JobStatus{Result: &proto.Result{
		Signal: "killed", SignalNum: 9, OutputsOK: true,
		CleanupOK: false, LogsComplete: false, TransactionError: "scope cleanup failed",
	}}, &stderr, "peer/job")
	if code != 137 {
		t.Fatalf("SIGKILL exit = %d, want 137", code)
	}
	for _, want := range []string{"cleanup failed", "logs truncated", "scope cleanup failed"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("signaled transaction report %q does not contain %q", stderr.String(), want)
		}
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
				Signal: "interrupt", SignalNum: 2, OutputsOK: true, CleanupOK: true, LogsComplete: true,
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
		done <- run(RunOptions{
			PeerURL: server.URL, Root: root, Argv: []string{"/bin/true"},
			Stdout: io.Discard, Stderr: io.Discard,
		}, interrupts, func() {})
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
	code := run(RunOptions{
		PeerURL: server.URL, Root: root, Argv: []string{"/bin/true"},
		Stdout: io.Discard, Stderr: io.Discard,
	}, interrupts, func() {})
	if code != 130 {
		t.Fatalf("pre-admission interrupt exit = %d, want 130", code)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("pre-admission interrupt made %d remote requests, want 0", got)
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
	done := make(chan struct{})
	go func() {
		forwardInterrupts(ctx, interrupts, server.URL, proto.NewULID(), "peer/job", func(string, ...any) {}, func() {
			close(reset)
		})
		close(done)
	}()
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
	case <-done:
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

func TestTerminalJobLogReplayFailureIsTransactionFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "log unavailable", http.StatusBadRequest)
	}))
	defer server.Close()
	code := 0
	_, err := stream(RunOptions{PeerURL: server.URL}, "job", proto.JobStatus{
		ID: "job", State: proto.StateExited,
		Result: &proto.Result{ExitCode: &code, OutputsOK: true, CleanupOK: true, LogsComplete: true},
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
	final, err := stream(RunOptions{PeerURL: server.URL}, "job", proto.JobStatus{
		ID: "job", State: proto.StateExited,
		Result: &proto.Result{ExitCode: &code, OutputsOK: true, CleanupOK: true, LogsComplete: true},
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
	_, err := stream(RunOptions{PeerURL: server.URL}, "job", proto.JobStatus{ID: "job", State: proto.StateRunning})
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
	_, err := stream(RunOptions{PeerURL: server.URL}, "job", proto.JobStatus{ID: "job", State: proto.StateRunning})
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
	final, err := stream(RunOptions{PeerURL: server.URL}, "job", proto.JobStatus{ID: "job", State: proto.StateRunning})
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
	_, err := stream(RunOptions{PeerURL: server.URL, Stdout: io.Discard, Stderr: io.Discard}, "job", proto.JobStatus{
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
	_, err := stream(RunOptions{PeerURL: server.URL, Stdout: shortWriter{}, Stderr: io.Discard}, "job", proto.JobStatus{
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

	_, err = submit(RunOptions{PeerURL: server.URL, Root: root}, "same", spec, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("submit attempts = %d, want 2", got)
	}
}
