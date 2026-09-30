//go:build windows

package daemon

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/lydakis/errand/internal/proctree"
)

// terminatedExitCode is the exit code TerminateJobObject gives processes the
// daemon kills, so the receipt can report the kill as a signal.
const terminatedExitCode = 0xE77A0009

// processScope is a Job Object on Windows. Every process the job leader
// starts joins it and cannot break away, so no environment or directory scan
// is needed. The Job Object kills its processes when the daemon exits, so a
// restarted daemon never has survivors to settle.
type processScope struct {
	group      *processGroupRecord
	groupOwned bool
	token      string

	mu     sync.Mutex
	job    *proctree.Job
	killed syscall.Signal
}

func newProcessScopeWithToken(token, _ string, _ ...string) (*processScope, error) {
	return &processScope{token: token}, nil
}

func (s *processScope) env() string {
	return processScopeEnv + "=" + s.token
}

func (s *processScope) prepare(cmd *exec.Cmd) {
	proctree.Prepare(cmd)
}

func (s *processScope) adopt(cmd *exec.Cmd) error {
	job, err := proctree.New()
	if err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	if err := job.Adopt(cmd.Process); err != nil {
		job.Close()
		return err
	}
	s.mu.Lock()
	s.job = job
	s.mu.Unlock()
	return nil
}

// signal ends the whole job. Windows has no signal a daemon without a console
// can deliver to a process tree, so SIGINT and SIGTERM also terminate it.
func (s *processScope) signal(_ int, sig syscall.Signal) (exited bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return true, nil
	}
	pids, err := s.job.PIDs()
	if err != nil {
		return false, err
	}
	if len(pids) == 0 {
		return true, nil
	}
	if s.killed == 0 {
		s.killed = sig
	}
	return false, s.job.Terminate(terminatedExitCode)
}

func (s *processScope) exitSignal(ws syscall.WaitStatus) (syscall.Signal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.killed != 0 && uint32(ws.ExitStatus()) == terminatedExitCode {
		return s.killed, true
	}
	return 0, false
}

func (s *processScope) pids() ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return nil, nil
	}
	return s.job.PIDs()
}

// cleanup terminates whatever the job leader left running, then releases the
// job. It returns the pids it found for the receipt.
func (s *processScope) cleanup(timeout time.Duration) ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return nil, nil
	}
	pids, err := s.job.PIDs()
	if err != nil {
		return nil, err
	}
	if len(pids) == 0 {
		return nil, s.closeLocked()
	}
	if err := s.job.Terminate(terminatedExitCode); err != nil {
		return pids, err
	}
	deadline := time.Now().Add(timeout)
	for {
		remaining, err := s.job.PIDs()
		if err != nil {
			return pids, err
		}
		if len(remaining) == 0 {
			return pids, s.closeLocked()
		}
		if time.Now().After(deadline) {
			return pids, fmt.Errorf("process scope still contains pids %v", remaining)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (s *processScope) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.closeLocked()
}

func (s *processScope) closeLocked() error {
	if s.job == nil {
		return nil
	}
	err := s.job.Close()
	s.job = nil
	return err
}
