//go:build windows

package unixpeer

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestCredentialsIdentifySameUserOnWindows(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- conn
	}()
	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}
	defer server.Close()
	pid, err := ProcessID(server.(*net.UnixConn))
	if err != nil {
		t.Fatal(err)
	}
	if pid != os.Getpid() {
		t.Fatalf("peer pid %d, want %d", pid, os.Getpid())
	}
	peer, err := Credentials(server.(*net.UnixConn))
	if err != nil {
		t.Fatal(err)
	}
	if peer.UID != CurrentUID() || peer.User == "" {
		t.Fatalf("peer %+v, want same user with a name", peer)
	}
	conn, err := Dial(t.Context(), socket, CurrentUID())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	conn.Close()
	if _, err := Dial(t.Context(), socket, OtherUser); err == nil {
		t.Fatal("Dial accepted a server for the wrong user")
	}
}

func TestStaleSocketIsConnectionRefusedOnWindows(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	// Leave the socket file behind, as a crashed daemon would.
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	listener.Close()
	info, err := os.Lstat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("stale socket mode %v is not a socket", info.Mode())
	}
	_, err = net.Dial("unix", socket)
	if !ConnectionRefused(err) {
		t.Fatalf("dial stale socket: %v, want connection refused", err)
	}
}

func TestMissingSocketsReportNoListenerOnWindows(t *testing.T) {
	dir := t.TempDir()
	for _, socket := range []string{
		filepath.Join(dir, "s"),
		filepath.Join(dir, "absent", "s"),
	} {
		_, err := net.Dial("unix", socket)
		if err == nil || !ConnectionRefused(err) {
			t.Fatalf("dial %s: %v, want no listener", socket, err)
		}
	}
}
