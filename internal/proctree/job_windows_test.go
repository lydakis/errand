//go:build windows

package proctree

import (
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"
)

// TestMain lets the test binary act as the contained program.
func TestMain(m *testing.M) {
	switch os.Getenv("PROCTREE_TEST_ROLE") {
	case "parent":
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "PROCTREE_TEST_ROLE=child")
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		os.Stdout.WriteString("started\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	case "child":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "exit7":
		os.Exit(7)
	}
	os.Exit(m.Run())
}

func startInJob(t *testing.T, role string) (*Job, *exec.Cmd) {
	t.Helper()
	job, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { job.Close() })
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "PROCTREE_TEST_ROLE="+role)
	Prepare(cmd)
	return job, cmd
}

func TestAdoptedProcessRunsAndExits(t *testing.T) {
	job, cmd := startInJob(t, "exit7")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := job.Adopt(cmd.Process); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 7 {
		t.Fatalf("wait: %v, exit %v", err, cmd.ProcessState)
	}
}

func TestTerminateKillsDescendants(t *testing.T) {
	job, cmd := startInJob(t, "parent")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := job.Adopt(cmd.Process); err != nil {
		t.Fatal(err)
	}
	line := make([]byte, len("started\n"))
	if _, err := stdout.Read(line); err != nil {
		t.Fatal(err)
	}
	// The parent, its child, and any console host Windows attaches.
	pids := waitForPIDs(t, job, func(n int) bool { return n >= 2 })
	if !slices.Contains(pids, cmd.Process.Pid) {
		t.Fatalf("job pids %v do not include parent %d", pids, cmd.Process.Pid)
	}
	if err := job.Terminate(9); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if code := cmd.ProcessState.ExitCode(); code != 9 {
		t.Fatalf("exit code %d, want 9", code)
	}
	waitForPIDs(t, job, func(n int) bool { return n == 0 })
}

func waitForPIDs(t *testing.T, job *Job, done func(int) bool) []int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		pids, err := job.PIDs()
		if err != nil {
			t.Fatal(err)
		}
		if done(len(pids)) {
			return pids
		}
		if time.Now().After(deadline) {
			t.Fatalf("job still has pids %v", pids)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
