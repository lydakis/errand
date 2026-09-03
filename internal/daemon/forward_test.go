package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

type endpointFunc func(context.Context, uint16) (net.Conn, error)

func (f endpointFunc) DialTCP(ctx context.Context, port uint16) (net.Conn, error) {
	return f(ctx, port)
}

func runningForwardJob(endpoint JobEndpoint) *Job {
	j := newJob(proto.NewULID(), "")
	j.state = proto.StateRunning
	j.endpoint = endpoint
	return j
}

func forwardTestServer(t *testing.T, j *Job) *httptest.Server {
	t.Helper()
	d := &Daemon{cfg: Config{InsecureNoAuth: true}, jobs: map[string]*Job{j.ID: j}}
	ts := httptest.NewServer(d.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestForwardStreamsOpaqueBytesInBothDirections(t *testing.T) {
	serverSide, endpointSide := net.Pipe()
	t.Cleanup(func() { _ = endpointSide.Close() })
	j := runningForwardJob(endpointFunc(func(_ context.Context, port uint16) (net.Conn, error) {
		if port != 4321 {
			t.Errorf("forward port = %d, want 4321", port)
			return nil, fmt.Errorf("unexpected port %d", port)
		}
		return serverSide, nil
	}))
	ts := forwardTestServer(t, j)

	requestReader, requestWriter := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v0/jobs/"+j.ID+"/ports/4321/connect", requestReader)
	if err != nil {
		t.Fatal(err)
	}
	response := make(chan *http.Response, 1)
	requestErr := make(chan error, 1)
	go func() {
		resp, err := ts.Client().Do(req)
		if err != nil {
			requestErr <- err
			return
		}
		response <- resp
	}()

	clientBytes := []byte{0, 1, 2, '\n', 0xff}
	go func() {
		_, _ = requestWriter.Write(clientBytes)
	}()
	gotClient := make([]byte, len(clientBytes))
	if _, err := io.ReadFull(endpointSide, gotClient); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotClient, clientBytes) {
		t.Fatalf("endpoint received %v, want %v", gotClient, clientBytes)
	}

	serverBytes := []byte{0xfe, 9, 8, 0, '\r'}
	go func() {
		_, _ = endpointSide.Write(serverBytes)
	}()
	var resp *http.Response
	select {
	case resp = <-response:
	case err := <-requestErr:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("forward response did not start")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forward status = %s", resp.Status)
	}
	gotServer := make([]byte, len(serverBytes))
	if _, err := io.ReadFull(resp.Body, gotServer); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotServer, serverBytes) {
		t.Fatalf("client received %v, want %v", gotServer, serverBytes)
	}
	_ = requestWriter.Close()
	_ = endpointSide.Close()
}

func TestForwardRemoteCloseEndsStreamingRequest(t *testing.T) {
	serverSide, endpointSide := net.Pipe()
	j := runningForwardJob(endpointFunc(func(context.Context, uint16) (net.Conn, error) {
		return serverSide, nil
	}))
	ts := forwardTestServer(t, j)

	requestReader, requestWriter := io.Pipe()
	defer requestWriter.Close()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v0/jobs/"+j.ID+"/ports/3000/connect", requestReader)
	if err != nil {
		t.Fatal(err)
	}
	response := make(chan *http.Response, 1)
	requestErr := make(chan error, 1)
	go func() {
		resp, err := ts.Client().Do(req)
		if err != nil {
			requestErr <- err
			return
		}
		response <- resp
	}()

	var resp *http.Response
	select {
	case resp = <-response:
	case err := <-requestErr:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("forward response did not start")
	}
	defer resp.Body.Close()
	if err := endpointSide.Close(); err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(make([]byte, 1))
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("forward response remained open after the remote endpoint closed")
		}
	case <-time.After(time.Second):
		t.Fatal("remote endpoint close did not finish the forwarding response")
	}
}

func TestHostJobEndpointDialsRunnerLoopback(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			accepted <- connection
		}
	}()
	connection, err := (hostJobEndpoint{}).DialTCP(context.Background(), uint16(listener.Addr().(*net.TCPAddr).Port))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	select {
	case peer := <-accepted:
		peer.Close()
	case <-time.After(time.Second):
		t.Fatal("host endpoint did not reach runner loopback")
	}
}

func TestForwardRefusesQueuedJob(t *testing.T) {
	dialed := false
	j := newJob(proto.NewULID(), "")
	j.state = proto.StateQueued
	j.endpoint = endpointFunc(func(context.Context, uint16) (net.Conn, error) {
		dialed = true
		return nil, fmt.Errorf("unexpected dial")
	})
	ts := forwardTestServer(t, j)
	resp, err := http.Post(ts.URL+"/v0/jobs/"+j.ID+"/ports/3000/connect", "application/octet-stream", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("queued forward status = %s, want 409", resp.Status)
	}
	if dialed {
		t.Fatal("queued job attempted to dial")
	}
}

func TestForwardRefusesTerminalJob(t *testing.T) {
	j := newJob(proto.NewULID(), "")
	j.state = proto.StateExited
	j.result = &proto.Result{State: proto.StateExited}
	close(j.done)
	close(j.executionDone)
	j.endpoint = endpointFunc(func(context.Context, uint16) (net.Conn, error) {
		t.Error("terminal job attempted to dial")
		return nil, fmt.Errorf("terminal job attempted to dial")
	})
	ts := forwardTestServer(t, j)
	resp, err := http.Post(ts.URL+"/v0/jobs/"+j.ID+"/ports/3000/connect", "application/octet-stream", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("terminal forward status = %s, want 409", resp.Status)
	}
}

func TestForwardDoesNotDialAfterExecutionEnds(t *testing.T) {
	dialed := make(chan struct{}, 1)
	j := newJob(proto.NewULID(), "")
	j.state = proto.StateRunning
	j.endpoint = endpointFunc(func(context.Context, uint16) (net.Conn, error) {
		dialed <- struct{}{}
		return nil, fmt.Errorf("unexpected dial")
	})
	ts := forwardTestServer(t, j)
	response := make(chan *http.Response, 1)
	requestErr := make(chan error, 1)
	go func() {
		resp, err := http.Post(ts.URL+"/v0/jobs/"+j.ID+"/ports/3000/connect", "application/octet-stream", http.NoBody)
		if err != nil {
			requestErr <- err
			return
		}
		response <- resp
	}()

	close(j.executionDone)
	select {
	case resp := <-response:
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("execution-end forward status = %s, want 409", resp.Status)
		}
	case err := <-requestErr:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("forward request did not finish")
	}
	select {
	case <-dialed:
		t.Fatal("forward dialed after execution ended")
	default:
	}
}

func TestForwardRejectsDifferentOwnerBeforeDial(t *testing.T) {
	dialed := false
	j := runningForwardJob(endpointFunc(func(context.Context, uint16) (net.Conn, error) {
		dialed = true
		return nil, fmt.Errorf("unexpected dial")
	}))
	j.Admission = proto.Admission{UserID: 1, UserLogin: "owner@example.com"}
	d := &Daemon{jobs: map[string]*Job{j.ID: j}}
	req := httptest.NewRequest(http.MethodPost, "/v0/jobs/"+j.ID+"/ports/3000/connect", http.NoBody)
	req.SetPathValue("id", j.ID)
	req.SetPathValue("port", "3000")
	w := httptest.NewRecorder()
	d.handleForward(w, req, Identity{UserID: 2, Login: "other@example.com"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("different-owner forward status = %d, want 403", w.Code)
	}
	if dialed {
		t.Fatal("different-owner forward reached the endpoint")
	}
}

func TestForwardDialIsCanceledWhenExecutionEnds(t *testing.T) {
	dialStarted := make(chan struct{})
	executionDone := make(chan struct{})
	j := runningForwardJob(endpointFunc(func(ctx context.Context, _ uint16) (net.Conn, error) {
		close(dialStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	j.executionDone = executionDone
	ts := forwardTestServer(t, j)
	response := make(chan *http.Response, 1)
	requestErr := make(chan error, 1)
	go func() {
		resp, err := http.Post(ts.URL+"/v0/jobs/"+j.ID+"/ports/3000/connect",
			"application/octet-stream", http.NoBody)
		if err != nil {
			requestErr <- err
			return
		}
		response <- resp
	}()
	select {
	case <-dialStarted:
	case <-time.After(time.Second):
		t.Fatal("endpoint dial did not begin")
	}
	close(executionDone)
	select {
	case resp := <-response:
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("execution-end forward status = %s, want 409", resp.Status)
		}
	case err := <-requestErr:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("forward dial was not canceled when execution ended")
	}
}

func TestActiveForwardClosesWhenExecutionEnds(t *testing.T) {
	serverSide, endpointSide := net.Pipe()
	defer endpointSide.Close()
	executionDone := make(chan struct{})
	j := runningForwardJob(endpointFunc(func(context.Context, uint16) (net.Conn, error) {
		return serverSide, nil
	}))
	j.executionDone = executionDone
	ts := forwardTestServer(t, j)
	resp, err := http.Post(ts.URL+"/v0/jobs/"+j.ID+"/ports/3000/connect",
		"application/octet-stream", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	close(executionDone)
	readDone := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(make([]byte, 1))
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("forward response remained readable after execution ended")
		}
	case <-time.After(time.Second):
		t.Fatal("active forward remained open after execution ended")
	}
}

func TestForwardReservationProtectsJobFromGC(t *testing.T) {
	j := &Job{done: make(chan struct{})}
	if !j.acquireForwardReader() {
		t.Fatal("could not acquire forward reader")
	}
	defer j.releaseForwardReader()
	settled := time.Now().Add(-time.Hour)
	j.state = proto.StateExited
	j.result = &proto.Result{
		State: proto.StateExited, SettledAt: &settled,
		ChangesOK: true, CleanupOK: true, LogsComplete: true,
	}
	if _, eligible := gcEligibleLocked(j); eligible {
		t.Fatal("job with an active forward was eligible for collection")
	}
}

func TestForwardEndpointIsDialedOncePerConnection(t *testing.T) {
	var mu sync.Mutex
	dials := 0
	j := runningForwardJob(endpointFunc(func(context.Context, uint16) (net.Conn, error) {
		mu.Lock()
		dials++
		mu.Unlock()
		left, right := net.Pipe()
		_ = right.Close()
		return left, nil
	}))
	ts := forwardTestServer(t, j)
	for range 2 {
		resp, err := http.Post(ts.URL+"/v0/jobs/"+j.ID+"/ports/3000/connect", "application/octet-stream", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if dials != 2 {
		t.Fatalf("endpoint dials = %d, want 2", dials)
	}
}
