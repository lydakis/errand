//go:build windows

package client

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detachWorker starts the worker without the launching console, so closing
// that console or pressing Ctrl-C there does not stop it.
func detachWorker(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}
