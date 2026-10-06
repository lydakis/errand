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
	"sync"
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

// streamClock is a clock for the follower's idle reads that moves only when
// a test advances it, so how long a stream was silent does not depend on
// how promptly the machine running the test schedules anything.
type streamClock struct {
	mu      sync.Mutex
	now     time.Duration
	reads   int // idle waits started so far
	pending []*streamWait
}

type streamWait struct {
	at      time.Duration
	expired chan time.Time
	done    bool
}

func useStreamClock(t *testing.T) *streamClock {
	t.Helper()
	c := &streamClock{}
	previous := idleTimer
	idleTimer = c.wait
	t.Cleanup(func() { idleTimer = previous })
	return c
}

func (c *streamClock) wait(d time.Duration) (<-chan time.Time, func() bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := &streamWait{at: c.now + d, expired: make(chan time.Time, 1)}
	c.reads++
	c.pending = append(c.pending, w)
	return w.expired, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		stopped := !w.done
		w.done = true
		return stopped
	}
}

// advance moves the clock on by d, ending the waits that run out, and
// returns how many reads had started by then.
func (c *streamClock) advance(d time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += d
	for _, w := range c.pending {
		if !w.done && w.at <= c.now {
			w.done = true
			w.expired <- time.Time{}
		}
	}
	return c.reads
}

// readsSince waits until a read starts after the one numbered n.
func (c *streamClock) readsSince(n int) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		reads := c.reads
		c.mu.Unlock()
		if reads > n {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func TestHeartbeatsKeepAQuietJobAttached(t *testing.T) {
	shortRunnerContact(t, 100*time.Millisecond, 200*time.Millisecond)
	clock := useStreamClock(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		// 600ms of silence from the job, well past idle and window, with a
		// heartbeat every 30ms. The clock moves on only once the follower
		// has taken the last heartbeat and is reading again, as a runner's
		// heartbeats would reach it in time.
		seen := 0
		for range 20 {
			io.WriteString(w, ":\n\n")
			w.(http.Flusher).Flush()
			if !clock.readsSince(seen) {
				t.Error("the follower stopped reading")
				return
			}
			seen = clock.advance(30 * time.Millisecond)
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

// Without heartbeats, the same silence on the same clock ends the stream,
// so the test above shows the heartbeats, not the clock, keep it open.
func TestStreamClockEndsASilentStream(t *testing.T) {
	shortRunnerContact(t, 100*time.Millisecond, time.Hour)
	clock := useStreamClock(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) > 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: status\ndata: {\"id\":\"job\",\"state\":\"exited\",\"result\":{}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		if !clock.readsSince(0) {
			t.Error("the follower never read")
			return
		}
		clock.advance(100 * time.Millisecond)
		<-r.Context().Done()
	}))
	defer server.Close()

	final, err := streamContext(context.Background(), RunOptions{PeerURL: server.URL}, "job",
		proto.JobStatus{ID: "job", State: proto.StateRunning})
	if err != nil || final.State != proto.StateExited || requests.Load() != 2 {
		t.Fatalf("final = %+v, %v after %d connections, want a reconnect", final, err, requests.Load())
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
	status, ok := terminalWithoutLogs(err)
	if !ok || status.State != proto.StateExited {
		t.Fatalf("terminal replay outage error = %v, want the known terminal status", err)
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

// A runner that answers but keeps failing the log stream is reachable, so the
// follower asks it for the job's state instead of calling it lost.
func TestStalledStreamFromALiveRunnerReportsItsState(t *testing.T) {
	shortRunnerContact(t, 100*time.Millisecond, 200*time.Millisecond)
	jobID := proto.NewULID()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/jobs/"+jobID:
			json.NewEncoder(w).Encode(proto.JobStatus{ID: jobID, State: proto.StateRunning})
		case strings.HasSuffix(r.URL.Path, "/logs"):
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: error\ndata: {\"message\":\"reading io.log: input/output error\",\"retryable\":true}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stderr bytes.Buffer
	code := attachWithDetachNotifications(AttachOptions{
		PeerURL: server.URL, PeerName: "cabal", JobID: jobID, Stdout: io.Discard, Stderr: &stderr,
	}, make(chan os.Signal, 2), testInterruptNotifications(), nil)
	if code != ExitTransaction {
		t.Fatalf("attach exit = %d, want %d", code, ExitTransaction)
	}
	out := stderr.String()
	for _, want := range []string{
		"the runner answers, but the job's log stream kept failing",
		"input/output error",
		"the runner reports the job as running",
		"errand attach cabal/" + jobID,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "lost contact") || strings.Contains(out, "state unknown") {
		t.Fatalf("a reachable runner was reported lost:\n%s", out)
	}
}

func TestStalledStreamWithUnknownJobSaysWhy(t *testing.T) {
	shortRunnerContact(t, 100*time.Millisecond, 200*time.Millisecond)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/logs") {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, `{"error":"no such job"}`, http.StatusNotFound)
	}))
	defer server.Close()

	_, err := streamContext(context.Background(), RunOptions{PeerURL: server.URL}, "job",
		proto.JobStatus{ID: "job", State: proto.StateRunning})
	var stalled *LogStreamStalledError
	if !errors.As(err, &stalled) || stalled.Status != nil || !IsNotFound(stalled.StatusErr) {
		t.Fatalf("stalled stream error = %#v", err)
	}
}

// A job that finished while its log could not be replayed keeps its own exit
// code; the missing output only fails an otherwise successful run.
func TestStalledStreamKeepsTheJobsExitCode(t *testing.T) {
	for _, tc := range []struct {
		remote, want int
	}{{7, 7}, {0, ExitTransaction}} {
		t.Run(fmt.Sprint(tc.remote), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			shortRunnerContact(t, 100*time.Millisecond, 200*time.Millisecond)
			jobID := proto.NewULID()
			code := tc.remote
			var finished atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v0/jobs/"+jobID:
					status := proto.JobStatus{ID: jobID, State: proto.StateRunning}
					if finished.Load() {
						status = proto.JobStatus{ID: jobID, State: proto.StateExited, Result: &proto.Result{
							State: proto.StateExited, Started: true, ExitCode: &code,
							ChangesOK: true, CleanupOK: true, LogsComplete: true,
						}}
					}
					json.NewEncoder(w).Encode(status)
				case strings.HasSuffix(r.URL.Path, "/logs"):
					finished.Store(true) // the job ends while its log can't be read
					http.Error(w, "busy", http.StatusServiceUnavailable)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			var stderr bytes.Buffer
			got := attachWithDetachNotifications(AttachOptions{
				PeerURL: server.URL, PeerName: "cabal", JobID: jobID, Stdout: io.Discard, Stderr: &stderr,
			}, make(chan os.Signal, 2), testInterruptNotifications(), nil)
			if got != tc.want {
				t.Fatalf("attach exit = %d, want %d; stderr:\n%s", got, tc.want, stderr.String())
			}
			if !strings.Contains(stderr.String(), "the job finished, but its output could not be replayed") {
				t.Fatalf("stderr does not explain the missing output:\n%s", stderr.String())
			}
		})
	}
}

func TestEscalationReportsOnlyTheLatestUnconfirmedRequest(t *testing.T) {
	var e escalation
	if e.unconfirmed() != escalateNone {
		t.Fatal("nothing requested, yet something is unconfirmed")
	}
	e.request(escalateSIGINT)
	e.confirm(escalateSIGINT)
	if e.unconfirmed() != escalateNone {
		t.Fatal("confirmed SIGINT reported as unconfirmed")
	}
	e.request(escalateForceKill)
	if e.unconfirmed() != escalateForceKill {
		t.Fatal("a confirmed SIGINT hid an unconfirmed force-kill")
	}
	e.confirm(escalateForceKill)
	e.confirm(escalateSIGINT) // a late SIGINT confirmation cannot lower it
	if e.unconfirmed() != escalateNone {
		t.Fatal("confirmed force-kill reported as unconfirmed")
	}
}
