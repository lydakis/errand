//go:build windows

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lydakis/errand/internal/proctree"
)

// executableFile reports whether path names a regular file Windows would run:
// one whose extension is listed in PATHEXT.
func executableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return false
	}
	pathext := os.Getenv("PATHEXT")
	if pathext == "" {
		pathext = ".COM;.EXE;.BAT;.CMD"
	}
	for _, candidate := range filepath.SplitList(pathext) {
		if strings.ToLower(candidate) == ext {
			return true
		}
	}
	return false
}

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
