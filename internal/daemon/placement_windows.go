//go:build windows

package daemon

import (
	"os/exec"

	"github.com/lydakis/errand/internal/proctree"
)

// probeProcess is a runtime probe running in its own Job Object.
type probeProcess struct{ job *proctree.Job }

func startProbe(cmd *exec.Cmd) (*probeProcess, error) {
	job, err := proctree.New()
	if err != nil {
		return nil, err
	}
	proctree.Prepare(cmd)
	cmd.Cancel = func() error { return job.Terminate(1) }
	if err := cmd.Start(); err != nil {
		job.Close()
		return nil, err
	}
	if err := job.Adopt(cmd.Process); err != nil {
		_ = cmd.Wait()
		job.Close()
		return nil, err
	}
	return &probeProcess{job: job}, nil
}

func (p *probeProcess) kill() {
	_ = p.job.Terminate(1)
	_ = p.job.Close()
}
