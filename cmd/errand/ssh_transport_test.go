package main

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/daemon"
)

func buildErrand(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "errand")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building errand: %v\n%s", err, out)
	}
	return bin
}

func TestListenUnixSocketRefusesLiveListener(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "errand-listen-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "errand.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	if replacement, err := listenUnixSocket(path); err == nil {
		replacement.Close()
		t.Fatal("replaced a live Unix listener")
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("original listener is no longer reachable: %v", err)
	}
	conn.Close()
}

func TestListenUnixSocketReplacesStaleSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "errand-stale-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "errand.sock")
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := listenUnixSocket(path)
	if err != nil {
		t.Fatalf("replacing stale Unix socket: %v", err)
	}
	listener.Close()
}

func TestSSHTransportEndToEnd(t *testing.T) {
	bin := buildErrand(t)
	stateDir := t.TempDir()
	d, err := daemon.New(daemon.Config{StateDir: stateDir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	sockDir, err := os.MkdirTemp("/tmp", "errand ssh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "errand.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: d.Handler(), ConnContext: daemon.ConnContext}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close(); listener.Close() })

	fakeBin := t.TempDir()
	fakeSSH := filepath.Join(fakeBin, "ssh")
	script := "#!/bin/sh\n" +
		"while [ $# -gt 0 ]; do case \"$1\" in --) shift; break;; -o) shift 2;; -*) shift;; *) break;; esac; done\n" +
		"[ \"$1\" = \"george@fake-runner\" ] || exit 91\n" +
		"shift\n" +
		"[ \"$1\" = \"/opt/errand _stdio --socket '" + socket + "'\" ] || exit 92\n" +
		"exec " + bin + " _stdio --socket '" + socket + "'\n"
	if err := os.WriteFile(fakeSSH, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	peer := client.ConfigureSSHPeer("ssh://george@fake-runner", "test", "/opt/errand", socket)
	info, err := client.Info(peer)
	if err != nil {
		t.Fatalf("info over ssh transport: %v", err)
	}
	if info.Version != "test" {
		t.Fatalf("info over ssh = %+v", info)
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("over ssh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := client.Run(client.RunOptions{
		PeerURL: peer, PeerName: "test", Root: root, Argv: []string{"/bin/cat", "hello.txt"},
		Stdout: &stdout, Stderr: &stderr,
	})
	if code != 0 {
		t.Fatalf("run over ssh exit = %d; stderr: %s", code, stderr.String())
	}
	if stdout.String() != "over ssh\n" {
		t.Fatalf("run over ssh stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "job test/") {
		t.Fatalf("handle should be ssh-qualified: %s", stderr.String())
	}

	jobs, err := client.List(peer)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("list over ssh = %v, %v", jobs, err)
	}
}
