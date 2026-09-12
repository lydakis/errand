package client

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

func TestAutomaticApplyInspectionDoesNotCreateState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	t.Setenv("XDG_STATE_HOME", root)
	issues, err := InterruptedAutomaticApplies()
	if err != nil || len(issues) != 0 {
		t.Fatalf("inspection = %v, %v", issues, err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("inspection created state: %v", err)
	}
}

func TestAutomaticApplyWorkerDoesNotMistakeInspectorForOwner(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	name := localAutomaticApplyWorkerLockName(localChangeKey("http://runner.test", proto.NewULID()))
	f, err := openLocalChangeLock(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	type result struct {
		acquired bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		unlock, acquired, err := tryAcquireLocalChangeLease(name)
		if acquired {
			unlock()
		}
		done <- result{acquired, err}
	}()
	select {
	case result := <-done:
		t.Fatalf("worker gave up while inspector held lock: %+v", result)
	case <-time.After(20 * time.Millisecond):
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if !result.acquired || result.err != nil {
			t.Fatalf("worker failed after inspection: %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not acquire released inspection lock")
	}
}

func TestAutomaticApplyStatusDistinguishesStoppedWorker(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	state := localChangeState{JobID: proto.NewULID(), PeerURL: "http://runner.test", Root: t.TempDir(),
		ManifestRoot: (proto.Manifest{}).RootHash(), SubmissionStarted: true, ApplyOnSuccess: true,
		AutomaticApply: automaticApplyRunning}
	if err := saveLocalChangeState(state); err != nil {
		t.Fatal(err)
	}
	root, _ := localChangeRoot()
	path := filepath.Join(root, "jobs", localChangeKey(state.PeerURL, state.JobID)+".json")
	before, _ := os.ReadFile(path)
	status, err := GetAutomaticApplyStatus(state.PeerURL, state.JobID)
	if err != nil || status.State != "needs_recovery" {
		t.Fatalf("stopped status = %+v, %v", status, err)
	}
	if _, err := os.Stat(filepath.Join(root, "locks")); !os.IsNotExist(err) {
		t.Fatalf("inspection created locks: %v", err)
	}
	unlock, acquired, err := tryAcquireLocalChangeLease(localAutomaticApplyWorkerLockName(localChangeKey(state.PeerURL, state.JobID)))
	if err != nil || !acquired {
		t.Fatalf("lease = %t, %v", acquired, err)
	}
	status, err = GetAutomaticApplyStatus(state.PeerURL, state.JobID)
	unlock()
	if err != nil || status.State != automaticApplyRunning {
		t.Fatalf("live status = %+v, %v", status, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("inspection rewrote persisted state")
	}
}

func TestWorkerLeaseKeepsOneInodeAcrossOwners(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	name := localAutomaticApplyWorkerLockName(localChangeKey("http://runner.test", proto.NewULID()))
	unlock, acquired, err := tryAcquireLocalChangeLease(name)
	if err != nil || !acquired {
		t.Fatalf("lease: %t, %v", acquired, err)
	}
	root, _ := localChangeRoot()
	observer, err := os.Open(filepath.Join(root, "locks", name+".lock"))
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	defer observer.Close()
	before, _ := observer.Stat()
	unlock()
	unlock, acquired, err = tryAcquireLocalChangeLease(name)
	if err != nil || !acquired {
		t.Fatalf("second lease: %t, %v", acquired, err)
	}
	defer unlock()
	after, err := os.Stat(observer.Name())
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("lease inode changed: %v", err)
	}
	if err := syscall.Flock(int(observer.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		t.Fatal("observer missed the replacement owner")
	}
}
