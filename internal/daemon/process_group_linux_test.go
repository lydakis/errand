package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
)

func TestProcessGroupScanHandlesDisappearingProcesses(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	for _, failure := range []syscall.Errno{syscall.ENOENT, syscall.ESRCH, syscall.EACCES} {
		t.Run(failure.Error(), func(t *testing.T) {
			injected := false
			snapshot, err := inspectProcessGroupWith(cmd.Process.Pid, func(path string) ([]byte, error) {
				if filepath.Base(filepath.Dir(path)) == strconv.Itoa(os.Getpid()) {
					injected = true
					return nil, &os.PathError{Op: "read", Path: path, Err: failure}
				}
				return os.ReadFile(path)
			})
			if !injected {
				t.Fatal("did not exercise process-stat failure")
			}
			if failure == syscall.EACCES {
				if !errors.Is(err, failure) {
					t.Fatalf("unexpected inspection failure was hidden: %v", err)
				}
			} else if err != nil || snapshot.leaderBirth == "" || !slices.Contains(snapshot.pids, cmd.Process.Pid) {
				t.Fatalf("unrelated disappearing process aborted group inspection: %+v %v", snapshot, err)
			}
		})
	}
}
