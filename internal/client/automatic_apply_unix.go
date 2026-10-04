//go:build unix

package client

import (
	"os/exec"
	"syscall"
)

// detachWorker starts the worker in its own session, so it survives the
// terminal that launched it.
func detachWorker(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
