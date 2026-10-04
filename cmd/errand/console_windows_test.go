//go:build windows

package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// A runner started by Task Scheduler owns a console that nobody reads and
// detaches from it. Its old standard handles must not reach handles opened
// later, which crashed the runner a job or two after it started. The helper
// runs in its own console, detaches, then churns handles, child processes and
// network I/O under constant garbage collection.
func TestDetachedRunnerKeepsWorkingOnWindows(t *testing.T) {
	if os.Getenv("ERRAND_DETACH_HELPER") == "1" {
		detachedRunnerHelper(t)
		return
	}
	logPath := filepath.Join(t.TempDir(), "errand.log")
	cmd := exec.Command(os.Args[0], "-test.run=^TestDetachedRunnerKeepsWorkingOnWindows$")
	cmd.Env = append(os.Environ(), "ERRAND_DETACH_HELPER=1", "GOGC=1", "ERRAND_DETACH_LOG="+logPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE}
	if err := cmd.Run(); err != nil {
		logged, _ := os.ReadFile(logPath)
		t.Fatalf("detached helper failed: %v\n%s", err, logged)
	}
}

func detachedRunnerHelper(t *testing.T) {
	f, err := os.OpenFile(os.Getenv("ERRAND_DETACH_LOG"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Start as Task Scheduler starts the runner: the standard handles are
	// the console's own. (exec gave this process NUL instead.)
	for _, std := range []struct {
		name   string
		handle uint32
		file   **os.File
	}{
		{"CONIN$", windows.STD_INPUT_HANDLE, &os.Stdin},
		{"CONOUT$", windows.STD_OUTPUT_HANDLE, &os.Stdout},
		{"CONOUT$", windows.STD_ERROR_HANDLE, &os.Stderr},
	} {
		console, err := os.OpenFile(std.name, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := windows.SetStdHandle(std.handle, windows.Handle(console.Fd())); err != nil {
			t.Fatal(err)
		}
		retiredStdio = append(retiredStdio, *std.file)
		*std.file = console
	}
	fail := func(format string, args ...any) {
		fmt.Fprintf(f, format+"\n", args...)
		os.Exit(1)
	}
	useServiceLog(f)
	if handle, _ := windows.GetStdHandle(windows.STD_ERROR_HANDLE); handle != windows.Handle(f.Fd()) {
		fail("stderr handle %v does not name the log %v", handle, f.Fd())
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fail("%v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(conn, conn); conn.Close() }()
		}
	}()
	for i := range 20 {
		runtime.GC()
		if err := exec.Command("cmd", "/d", "/c", "exit 0").Run(); err != nil {
			fail("job %d: %v", i, err)
		}
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			fail("dial %d: %v", i, err)
		}
		want := fmt.Sprintf("round %d", i)
		if _, err := io.WriteString(conn, want); err != nil {
			fail("%v", err)
		}
		got := make([]byte, len(want))
		if _, err := io.ReadFull(conn, got); err != nil || string(got) != want {
			fail("echo %d = %q, %v", i, got, err)
		}
		conn.Close()
	}
}
