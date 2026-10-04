//go:build !windows

package nowindow

import "os/exec"

func Hide(*exec.Cmd) {}
