//go:build windows

package nowindow

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Hide starts cmd's program without a console window.
func Hide(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
