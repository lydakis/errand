//go:build unix

package daemon

import (
	"os/exec"
	"syscall"
)

// probeProcess is a runtime probe running in its own process group.
type probeProcess struct{ pid int }

func startProbe(cmd *exec.Cmd) (*probeProcess, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &probeProcess{pid: cmd.Process.Pid}, nil
}

func (p *probeProcess) kill() {
	_ = syscall.Kill(-p.pid, syscall.SIGKILL)
}
