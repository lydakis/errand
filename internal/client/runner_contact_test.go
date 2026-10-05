package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

// shortRunnerContact shrinks the follower's timing so tests can watch a
// runner go quiet without waiting real minutes.
func shortRunnerContact(t *testing.T, idle, window time.Duration) {
	t.Helper()
	previousIdle, previousWindow, previousBackoff := logStreamIdleTimeout, runnerContactWindow, reconnectBackoff
	logStreamIdleTimeout, runnerContactWindow = idle, window
	reconnectBackoff = func(int) time.Duration { return 10 * time.Millisecond }
	t.Cleanup(func() {
		logStreamIdleTimeout, runnerContactWindow, reconnectBackoff = previousIdle, previousWindow, previousBackoff
	})
}

func logFrame(seq int64, data string) string {
	b, _ := json.Marshal(proto.LogFrame{Seq: seq, Stream: "stdout", DataB64: base64.StdEncoding.EncodeToString([]byte(data))})
	return fmt.Sprintf("id: %d\nevent: log\ndata: %s\n\n", seq, b)
}

func TestHeartbeatsKeepAQuietJobAttached(t *testing.T) {
	shortRunnerContact(t, 100*time.Millisecond, 200*time.Millisecond)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		for range 20 { // 600ms of silence from the job, well past idle and window
			io.WriteString(w, ":\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(30 * time.Millisecond)
		}
		io.WriteString(w, "event: status\ndata: {\"id\":\"job\",\"state\":\"exited\",\"result\":{}}\n\n")
	}))
	defer server.Close()

	final, err := streamContext(context.Background(), RunOptions{PeerURL: server.URL}, "job",
		proto.JobStatus{ID: "job", State: proto.StateRunning})
	if err != nil {
		t.Fatal(err)
	}
	if final.State != proto.StateExited || requests.Load() != 1 {
		t.Fatalf("final = %+v after %d connections, want one uninterrupted stream", final, requests.Load())
	}
}

func TestRunnerThatGoesSilentIsReportedUnavailable(t *testing.T) {
	shortRunnerContact(t, 100*time.Millisecond, 300*time.Millisecond)
	var dead atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dead.Load() {
			panic(http.ErrAbortHandler)
		}
		dead.Store(true)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, logFrame(1, "working\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // the host froze: the connection stays open, nothing arrives
	}))
	defer server.Close()

	started := time.Now()
	var stdout bytes.Buffer
	_, err := streamContext(context.Background(), RunOptions{PeerURL: server.URL, Stdout: &stdout}, "job",
		proto.JobStatus{ID: "job", State: proto.StateRunning})
	elapsed := time.Since(started)
	var unavailable *RunnerUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("silent runner error = %v, want RunnerUnavailableError", err)
	}
	if unavailable.For < runnerContactWindow {
		t.Fatalf("gave up after %s of failed contact, window is %s", unavailable.For, runnerContactWindow)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("follower waited %s for a silent runner", elapsed)
	}
	if stdout.String() != "working\n" {
		t.Fatalf("output before the runner went quiet = %q", stdout.String())
	}
}

func TestUnreachableRunnerIsReportedUnavailable(t *testing.T) {
	shortRunnerContact(t, 100*time.Millisecond, 200*time.Millisecond)
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close() // nothing listens there now

	_, err := streamContext(context.Background(), RunOptions{PeerURL: url}, "job",
		proto.JobStatus{ID: "job", State: proto.StateQueued})
	var unavailable *RunnerUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("unreachable runner error = %v, want RunnerUnavailableError", err)
	}
}

func TestRunnerBackWithinWindowResumesWhereItLeftOff(t *testing.T) {
	shortRunnerContact(t, 100*time.Millisecond, 2*time.Second)
	var requests atomic.Int32
	var resumedFrom atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, logFrame(1, "one\n"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case 2, 3:
			panic(http.ErrAbortHandler) // restarting
		default:
			resumedFrom.Store(r.URL.Query().Get("from"))
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, logFrame(2, "two\n"))
			io.WriteString(w, "event: status\ndata: {\"id\":\"job\",\"state\":\"exited\",\"result\":{}}\n\n")
		}
	}))
	defer server.Close()

	var stdout bytes.Buffer
	final, err := streamContext(context.Background(), RunOptions{PeerURL: server.URL, Stdout: &stdout}, "job",
		proto.JobStatus{ID: "job", State: proto.StateRunning})
	if err != nil {
		t.Fatal(err)
	}
	if final.State != proto.StateExited || stdout.String() != "one\ntwo\n" || resumedFrom.Load() != "1" {
		t.Fatalf("final = %+v, output = %q, resumed from %v", final, stdout.String(), resumedFrom.Load())
	}
}

// Contact resets the window: a runner that keeps coming back between short
// outages is never declared unavailable.
func TestEachContactRestartsTheWindow(t *testing.T) {
	shortRunnerContact(t, 50*time.Millisecond, 150*time.Millisecond)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 8 {
			io.WriteString(w, "event: status\ndata: {\"id\":\"job\",\"state\":\"exited\",\"result\":{}}\n\n")
			return
		}
		io.WriteString(w, ":\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(60 * time.Millisecond) // then the connection drops
		panic(http.ErrAbortHandler)
	}))
	defer server.Close()

	if _, err := streamContext(context.Background(), RunOptions{PeerURL: server.URL}, "job",
		proto.JobStatus{ID: "job", State: proto.StateRunning}); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalReplayOutageIsNotUnknownState(t *testing.T) {
	shortRunnerContact(t, 50*time.Millisecond, 100*time.Millisecond)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	code := 0
	_, err := streamContext(context.Background(), RunOptions{PeerURL: server.URL}, "job", proto.JobStatus{
		ID: "job", State: proto.StateExited, Result: &proto.Result{ExitCode: &code},
	})
	var unavailable *RunnerUnavailableError
	if err == nil || errors.As(err, &unavailable) || !strings.Contains(err.Error(), "terminal log replay") {
		t.Fatalf("terminal replay outage error = %v", err)
	}
}

func TestAttachToARunnerThatDiesSaysStateUnknown(t *testing.T) {
	shortRunnerContact(t, 100*time.Millisecond, 200*time.Millisecond)
	jobID := proto.NewULID()
	var dead atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case dead.Load():
			panic(http.ErrAbortHandler)
		case r.Method == http.MethodGet && r.URL.Path == "/v0/jobs/"+jobID:
			json.NewEncoder(w).Encode(proto.JobStatus{ID: jobID, State: proto.StateRunning})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/logs"):
			dead.Store(true)
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, logFrame(1, "compiling\n"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- attachWithDetachNotifications(AttachOptions{
			PeerURL: server.URL, PeerName: "cabal", JobID: jobID, Stdout: &stdout, Stderr: &stderr,
		}, make(chan os.Signal, 2), testInterruptNotifications(), nil)
	}()
	var code int
	select {
	case code = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("attach kept waiting on a dead runner")
	}
	if code != ExitTransaction {
		t.Fatalf("attach exit = %d, want %d; stderr: %s", code, ExitTransaction, stderr.String())
	}
	out := stderr.String()
	handle := "cabal/" + jobID
	for _, want := range []string{
		"lost contact with the runner",
		"job state unknown",
		"errand status " + handle,
		"errand attach " + handle,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Ctrl-C") {
		t.Fatalf("no interrupt was sent, but stderr mentions one:\n%s", out)
	}
}

func TestUnconfirmedInterruptIsReportedWhenRunnerIsLost(t *testing.T) {
	shortRunnerContact(t, 100*time.Millisecond, 300*time.Millisecond)
	jobID := proto.NewULID()
	logStarted := make(chan struct{})
	var dead atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case dead.Load():
			panic(http.ErrAbortHandler)
		case r.Method == http.MethodGet && r.URL.Path == "/v0/jobs/"+jobID:
			json.NewEncoder(w).Encode(proto.JobStatus{ID: jobID, State: proto.StateRunning})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/logs"):
			dead.Store(true)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			close(logStarted)
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	sigCh := make(chan os.Signal, 2)
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- attachWithDetachNotifications(AttachOptions{
			PeerURL: server.URL, PeerName: "cabal", JobID: jobID, Stdout: io.Discard, Stderr: &stderr,
		}, sigCh, testInterruptNotifications(), nil)
	}()
	<-logStarted
	sigCh <- os.Interrupt
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("attach kept waiting on a dead runner")
	}
	if !strings.Contains(stderr.String(), "Ctrl-C was not confirmed by the runner; stop the job with: errand kill cabal/"+jobID) {
		t.Fatalf("stderr does not admit the interrupt was lost:\n%s", stderr.String())
	}
}
