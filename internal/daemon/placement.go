package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/lydakis/errand/internal/placement"
	"github.com/lydakis/errand/internal/proto"
)

func executableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && canExecute(path)
}

// Relative entries depend on the future workspace, so cannot be attested.
func placementTool(tool string, env []string) string {
	check := executableFinder(envValue(env, "PATHEXT"))
	for _, dir := range filepath.SplitList(envValue(env, "PATH")) {
		if !filepath.IsAbs(dir) {
			continue
		}
		path := filepath.Join(dir, tool)
		if path, ok := check(path); ok {
			return path
		}
	}
	return ""
}

// Tool lookup observes the job PATH without executing it. Runtime liveness uses
// only the daemon's base environment and executable. Submitted environment must
// never control a pre-admission subprocess, even when its executable is trusted.
func (d *Daemon) measurePlacementFacts(ctx context.Context, q placement.Requirements, env []string) proto.Facts {
	f := measureFacts()
	f.ToolErrors = make(map[string]string)
	baseEnv := (&Job{}).buildEnv()
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, tool := range q.Tools {
		delete(f.Tools, tool)
	}
	for _, tool := range q.Tools {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path := placementTool(tool, env)
			var reason string
			if path == "" {
				reason = "not executable in absolute PATH entries"
			} else if tool == "docker" || tool == "podman" {
				trusted := placementTool(tool, baseEnv)
				jobFile, jobErr := os.Stat(path)
				baseFile, baseErr := os.Stat(trusted)
				if jobErr != nil || baseErr != nil || !os.SameFile(jobFile, baseFile) {
					reason = "job PATH does not resolve to the daemon's runtime"
				} else if err := d.probeRuntime(ctx, trusted, baseEnv); err != nil {
					reason = err.Error()
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if reason != "" {
				f.ToolErrors[tool] = reason
			} else {
				f.Tools[tool] = path
			}
		}()
	}
	wg.Wait()
	return f
}

// Bound subprocesses across info, submission and workspace-creation requests.
// Probes run concurrently so a slow Docker does not starve Podman.
func (d *Daemon) probeRuntime(ctx context.Context, path string, env []string) error {
	select {
	case d.placementSlots <- struct{}{}:
		defer func() { <-d.placementSlots }()
	case <-ctx.Done():
		return fmt.Errorf("runtime probe deadline exceeded waiting for capacity")
	}
	cmd := exec.CommandContext(ctx, path, "info")
	cmd.Env = env
	cmd.Dir = "/"
	cmd.WaitDelay = 100 * time.Millisecond
	probe, err := startProbe(cmd)
	if err != nil {
		return fmt.Errorf("runtime probe could not start")
	}
	err = cmd.Wait()
	// Remove any children retained by a runtime connection helper.
	probe.kill()
	if ctx.Err() != nil {
		return fmt.Errorf("runtime probe deadline exceeded")
	}
	if err != nil {
		return fmt.Errorf("runtime info failed")
	}
	return nil
}
