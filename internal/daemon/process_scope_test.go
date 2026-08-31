package daemon

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

func TestLinuxProcessScopeFindsMarkerAndWorkingDirectoryInProc(t *testing.T) {
	procRoot := t.TempDir()
	workdir := t.TempDir()
	for _, pid := range []int{101, 102, 103} {
		if err := os.Mkdir(filepath.Join(procRoot, strconv.Itoa(pid)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	marker := processScopeEnv + "=test-token"
	if err := os.WriteFile(filepath.Join(procRoot, "101", "environ"), []byte("A=B\x00"+marker+"\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procRoot, "102", "environ"), []byte("A=B\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procRoot, "103", "environ"), []byte("OTHER="+marker+"\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(workdir, filepath.Join(procRoot, "102", "cwd")); err != nil {
		t.Fatal(err)
	}

	scope := &processScope{token: "test-token", workdir: workdir, procRoot: procRoot}
	pids, err := scope.linuxPIDs()
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(pids)
	if got, want := fmt.Sprint(pids), "[101 102]"; got != want {
		t.Fatalf("linux scoped pids = %s, want %s", got, want)
	}
}

func TestResumeProcessScopeRejectsMalformedTokens(t *testing.T) {
	for _, token := range []string{
		"",
		"ab",
		strings.Repeat("a", 31),
		strings.Repeat("A", 32),
		strings.Repeat("z", 32),
	} {
		t.Run(fmt.Sprintf("%q", token), func(t *testing.T) {
			if _, err := resumeProcessScope(token, t.TempDir()); err == nil {
				t.Fatalf("resumeProcessScope accepted malformed token %q", token)
			}
		})
	}
}

func TestProcessScopeCleansDescendantThatCallsSetsid(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for the setsid regression")
	}
	workdir := t.TempDir()
	scope, err := newProcessScope(workdir)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.cleanup(2 * time.Second)
	pidFile := t.TempDir() + "/pid"
	script := `
import os, subprocess, sys
p = subprocess.Popen(
    [sys.executable, "-c", "import os,time; os.setsid(); time.sleep(30)"],
    stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    close_fds=True, env={},
)
open(sys.argv[1], "w").write(str(p.pid))
`
	cmd := exec.Command(python, "-c", script, pidFile)
	cmd.Env = append(os.Environ(), scope.env())
	cmd.Dir = workdir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("spawn escaped descendant: %v: %s", err, out)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("escaped descendant was not running before cleanup: %v", err)
	}
	killed, err := scope.cleanup(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(killed, pid) {
		t.Fatalf("cleanup killed %v, expected it to report escaped pid %d", killed, pid)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("escaped descendant pid %d survived cleanup: %v", pid, err)
}

func TestTopLevelExitCleansSetsidDescendantHoldingLogsOpen(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for the setsid regression")
	}
	d, err := New(Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	id := proto.NewULID()
	dir := filepath.Join(d.jobsDir(), id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	script := `
import subprocess, sys
subprocess.Popen(
    [sys.executable, "-c", "import os,time; os.setsid(); print('ready', flush=True); time.sleep(30)"],
    env={},
)
`
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{python, "-c", script},
		Limits: proto.Limits{
			MaxLogBytes: 1 << 20, MaxRuntimeSec: 2, MaxWorkspaceBytes: 1 << 20, MaxOutputBytes: 1 << 20,
		},
	}
	j := &Job{
		ID: id, Dir: dir, Spec: spec, RequestDigest: spec.Digest(),
		Admission: proto.Admission{Method: "insecure-test"},
		state:     proto.StateStaging, done: make(chan struct{}), logReady: make(chan struct{}),
	}
	d.mu.Lock()
	d.jobs[id] = j
	d.queue = append(d.queue, j)
	d.mu.Unlock()
	var workspace bytes.Buffer
	tw := tar.NewWriter(&workspace)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	settled, err := j.stage(d, io.NopCloser(&workspace), proto.Manifest{})
	if err != nil {
		t.Fatal(err)
	}
	if settled {
		t.Fatal("scope test job settled during staging")
	}
	if cancelled, err := d.queueStaged(j); err != nil {
		t.Fatal(err)
	} else if cancelled {
		t.Fatal("scope test job was cancelled before launch")
	}
	select {
	case <-j.done:
	case <-time.After(5 * time.Second):
		t.Fatal("top-level exit did not clean the descendant and release the job")
	}
	status := j.Status()
	if status.Result == nil || status.Result.LimitExceeded != "" || !status.Result.CleanupOK {
		t.Fatalf("top-level exit cleanup result = %+v", status.Result)
	}
}
