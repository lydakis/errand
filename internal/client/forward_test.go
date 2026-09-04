package client

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func unusedTCPPort(t *testing.T) uint16 {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestBindPortForwardsIsAtomic(t *testing.T) {
	first := unusedTCPPort(t)
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	second := uint16(occupied.Addr().(*net.TCPAddr).Port)

	session, err := bindPortForwards([]PortForward{
		{Local: first, Remote: 3000},
		{Local: second, Remote: 4000},
	}, io.Discard)
	if err == nil || session != nil {
		t.Fatalf("atomic bind = %+v, %v; want an error", session, err)
	}
	rebound, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(first))))
	if err != nil {
		t.Fatalf("first port remained bound after later bind failed: %v", err)
	}
	rebound.Close()
}

func TestBindPortForwardsRejectsIPv6LoopbackCollisionAtomically(t *testing.T) {
	occupied, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer occupied.Close()
	port := uint16(occupied.Addr().(*net.TCPAddr).Port)
	session, err := bindPortForwards([]PortForward{{Local: port, Remote: 3000}}, io.Discard)
	if err == nil || session != nil {
		t.Fatalf("IPv6 collision bind = %+v, %v; want an error", session, err)
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	rebound, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatalf("IPv4 listener remained after IPv6 bind failed: %v", err)
	}
	rebound.Close()
}

func TestRunBindFailurePreventsSubmission(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := uint16(occupied.Addr().(*net.TCPAddr).Port)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	code := runWithDetachNotifications(RunOptions{
		PeerURL: server.URL, Root: t.TempDir(), Argv: []string{"true"}, NoSnapshot: true,
		Forwards: []PortForward{{Local: port, Remote: 3000}}, Stdout: io.Discard, Stderr: io.Discard,
	}, make(chan os.Signal, 2), testInterruptNotifications(), nil)
	if code != ExitTransaction {
		t.Fatalf("bind failure exit = %d, want %d", code, ExitTransaction)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("bind failure made %d runner requests", got)
	}
}

func TestAttachBindFailurePreventsStatusRequest(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := uint16(occupied.Addr().(*net.TCPAddr).Port)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	code := attachWithDetachNotifications(AttachOptions{
		PeerURL: server.URL, JobID: "01K4Q8ZJ2M0000000000000000",
		Forwards: []PortForward{{Local: port, Remote: 3000}}, Stdout: io.Discard, Stderr: io.Discard,
	}, make(chan os.Signal, 2), testInterruptNotifications(), nil)
	if code != ExitTransaction {
		t.Fatalf("bind failure exit = %d, want %d", code, ExitTransaction)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("bind failure made %d runner requests", got)
	}
}

func TestForwardSessionCarriesOpaqueTrafficAndClosesOnDetach(t *testing.T) {
	jobID := "01K4Q8ZJ2M0000000000000000"
	requestSeen := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v0/jobs/"+jobID+"/ports/4321/connect" {
			http.NotFound(w, r)
			return
		}
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Error(err)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		requestSeen <- struct{}{}
		payload := make([]byte, 5)
		if _, err := io.ReadFull(r.Body, payload); err != nil {
			return
		}
		_, _ = w.Write(payload)
		w.(http.Flusher).Flush()
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer server.Close()

	localPort := unusedTCPPort(t)
	var stderr bytes.Buffer
	session, err := bindPortForwards([]PortForward{{Local: localPort, Remote: 4321}}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	session.Start(server.URL, jobID)
	local, err := net.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(localPort))))
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte{0, 1, 2, 0xff, '\n'}
	if _, err := local.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(local, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("round trip = %v, want %v", got, payload)
	}
	select {
	case <-requestSeen:
	case <-time.After(time.Second):
		t.Fatal("runner tunnel request was not observed")
	}
	session.Close()
	_ = local.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := local.Read(make([]byte, 1)); err == nil {
		t.Fatal("local connection remained open after the forwarding session closed")
	}
}

func TestForwardSessionUsesSSHTransport(t *testing.T) {
	jobID := "01K4Q8ZJ2M0000000000000000"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/jobs/"+jobID+"/ports/4321/connect" {
			http.NotFound(w, r)
			return
		}
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Error(err)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		payload := make([]byte, 4)
		if _, err := io.ReadFull(r.Body, payload); err != nil {
			return
		}
		_, _ = w.Write(payload)
		w.(http.Flusher).Flush()
	}))
	defer server.Close()

	oldDial := dialSSHConnection
	dialSSHConnection = func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	t.Cleanup(func() { dialSSHConnection = oldDial })

	localPort := unusedTCPPort(t)
	session, err := bindPortForwards([]PortForward{{Local: localPort, Remote: 4321}}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	session.Start("ssh://forward-test", jobID)
	local, err := net.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(localPort))))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	payload := []byte("ping")
	if _, err := local.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(local, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("ssh forward round trip = %q, want %q", got, payload)
	}
}

func TestForwardSessionAcceptsIPv6LoopbackConnections(t *testing.T) {
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	probe.Close()

	jobID := "01K4Q8ZJ2M0000000000000000"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Error(err)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		payload := make([]byte, 4)
		if _, err := io.ReadFull(r.Body, payload); err != nil {
			return
		}
		_, _ = w.Write(payload)
		w.(http.Flusher).Flush()
	}))
	defer server.Close()

	localPort := unusedTCPPort(t)
	session, err := bindPortForwards([]PortForward{{Local: localPort, Remote: 4321}}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	session.Start(server.URL, jobID)
	local, err := net.Dial("tcp6", net.JoinHostPort("::1", strconv.Itoa(int(localPort))))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	payload := []byte{0, 1, 0xff, '\n'}
	if _, err := local.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(local, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("IPv6 round trip = %v, want %v", got, payload)
	}
}
