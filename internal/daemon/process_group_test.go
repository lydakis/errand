package daemon

import (
	"os/exec"
	"slices"
	"syscall"
	"testing"
)

func TestProcessGroupRejectsReusedLeaderIdentity(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	group, err := captureProcessGroup(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	pids, err := group.members(false)
	if err != nil || !slices.Contains(pids, cmd.Process.Pid) {
		t.Fatalf("live group identity: %v %v", pids, err)
	}
	group.Birth += "-different-generation"
	if _, err := group.members(false); err == nil {
		t.Fatal("accepted another generation of the group leader")
	}
}

func TestProcessGroupProtectsLeaderlessSurvivorsAfterRestart(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 60 &")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	group, err := captureProcessGroup(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Wait()
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// The live daemon owns the group it just launched. A restarted daemon
	// cannot infer that ownership when the leader's identity is unavailable.
	pids, err := group.members(true)
	if err != nil || len(pids) == 0 {
		t.Fatalf("missing background child: %v %v", pids, err)
	}
	if _, err := group.members(false); err == nil {
		t.Fatal("accepted unverifiable leaderless group after restart")
	}
}

func TestSharedWorkspaceMissingGroupKeepsLease(t *testing.T) {
	scope, err := newProcessScope("")
	if err != nil {
		t.Fatal(err)
	}
	j := newJob("interrupted-launch", t.TempDir())
	if err := j.writeJSON("scope.json", scopeRecord{Token: scope.token, SharedWorkspace: true}); err != nil {
		t.Fatal(err)
	}
	released := false
	j.returnWorkspace = func() error { released = true; return nil }
	if _, errs := cleanupPersistedRuntime(j); len(errs) == 0 || released {
		t.Fatalf("released a lease after interrupted group publication: %v", errs)
	}
}

func TestProcessGroupPreviousBootDoesNotBlockCleanupOrKillReusedPID(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	group, err := captureProcessGroup(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if group.Boot == "" {
		t.Fatal("captured group lacks boot identity")
	}
	group.Boot, group.Birth = "previous-boot", "previous-process"
	scope, err := newProcessScope("")
	if err != nil {
		t.Fatal(err)
	}
	j := newJob("rebooted-job", t.TempDir())
	if err := j.writeJSON("scope.json", scopeRecord{Token: scope.token, SharedWorkspace: true, Group: group}); err != nil {
		t.Fatal(err)
	}
	released := false
	j.returnWorkspace = func() error { released = true; return nil }
	if killed, errs := cleanupPersistedRuntime(j); len(errs) != 0 || len(killed) != 0 || !released {
		t.Fatalf("previous-boot cleanup: killed=%v errors=%v released=%v", killed, errs, released)
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("cleanup killed a process from this boot: %v", err)
	}
}
