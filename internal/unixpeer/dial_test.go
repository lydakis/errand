//go:build linux || darwin

package unixpeer

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDialAuthenticatesServerBeforeSendingData(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "errand-peer-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// A mismatched expected UID exercises the real kernel credential rejection
	// without requiring root to create a second OS account in the test suite.
	for _, match := range []bool{false, true} {
		received := make(chan string, 1)
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				received <- err.Error()
				return
			}
			defer conn.Close()
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			b, err := io.ReadAll(conn)
			if err != nil {
				received <- err.Error()
				return
			}
			received <- string(b)
		}()
		uid := CurrentUID()
		if !match {
			uid++
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := Dial(ctx, socket, uid)
		cancel()
		want := ""
		if match {
			if err != nil {
				t.Fatal(err)
			}
			want = "authorized"
			if _, err := conn.Write([]byte(want)); err != nil {
				t.Fatal(err)
			}
			conn.Close()
		} else if err == nil || !strings.Contains(err.Error(), "does not match") || conn != nil {
			t.Fatalf("accepted wrong server identity: %v", err)
		}
		if got := <-received; got != want {
			t.Fatalf("server received %q, want %q", got, want)
		}
	}
}
