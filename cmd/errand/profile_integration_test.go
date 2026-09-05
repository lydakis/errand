package main

import (
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/daemon"
)

// Exercise one selected profile through a real daemon, process, port forward,
// ignored-output retention, and automatic application into the caller's tree.
func TestCombinedProfileEndToEnd(t *testing.T) {
	if os.Getenv("ERRAND_PROFILE_HELPER") == "1" {
		listener, err := net.Listen("tcp4", "127.0.0.1:"+os.Getenv("ERRAND_PROFILE_PORT"))
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second))
		conn, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		var request [1]byte
		if _, err := io.ReadFull(conn, request[:]); err != nil {
			t.Fatal(err)
		}
		value := os.Getenv("ERRAND_PROFILE_VALUE")
		if value != "profile-value" {
			t.Fatalf("forwarded value = %q", value)
		}
		if err := os.Mkdir("reports", 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("reports/result.txt", []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("source.txt", []byte("updated"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("ERRAND_PROFILE_VALUE", "profile-value")
	d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	server := httptest.NewServer(d.Handler())
	t.Cleanup(server.Close)
	writeClientConfig(t, fmt.Sprintf("default_peer = 'absent'\n[peers.build]\nurl = %q\n", server.URL))
	ports := make([]string, 2)
	reservations := make([]net.Listener, 2)
	for i := range ports {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ports[i] = strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
		reservations[i] = listener
		defer listener.Close()
	}
	// Release the reservations before the client and helper bind their ports.
	// The short bind race is bounded by the test's connection deadline.
	for _, listener := range reservations {
		_ = listener.Close()
	}
	t.Chdir(t.TempDir())
	marker := fmt.Sprintf(`[profiles.integration.run]
peer = "build"
[profiles.integration.env]
set = { ERRAND_PROFILE_HELPER = "1", ERRAND_PROFILE_PORT = "%s" }
pass = ["ERRAND_PROFILE_VALUE"]
[profiles.integration.session]
forward = ["%s:%s"]
[profiles.integration.artifacts]
paths = ["reports"]
[profiles.integration.changes]
apply_on_success = true
`, ports[1], ports[0], ports[1])
	for name, value := range map[string]string{".errand.toml": marker, ".errandignore": "reports/\n", "source.txt": "original"} {
		if err := os.WriteFile(name, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	forwarded := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			conn, err := net.DialTimeout("tcp4", "127.0.0.1:"+ports[0], 100*time.Millisecond)
			if err == nil {
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				_, err = conn.Write([]byte("x"))
				response := make([]byte, len("profile-value"))
				if err == nil {
					_, err = io.ReadFull(conn, response)
				}
				_ = conn.Close()
				if err == nil {
					if string(response) != "profile-value" {
						err = fmt.Errorf("forward response = %q", response)
					}
					forwarded <- err
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		forwarded <- fmt.Errorf("profile forward never served the helper response")
	}()
	code := cmdRun([]string{"--profile", "integration", "--", executable, "-test.run=^TestCombinedProfileEndToEnd$"})
	if err := <-forwarded; err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("profile run exit = %d", code)
	}
	for name, want := range map[string]string{"source.txt": "updated", "reports/result.txt": "profile-value"} {
		got, err := os.ReadFile(filepath.FromSlash(name))
		if err != nil || string(got) != want {
			t.Fatalf("applied %s = %q, %v", name, got, err)
		}
	}
}
