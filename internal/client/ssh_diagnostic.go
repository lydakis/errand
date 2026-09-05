package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type SSHDiagnosticError struct {
	CommandUnavailable bool
	Detail             string
}

func (e *SSHDiagnosticError) Error() string { return e.Detail }

// InspectSSH checks the configured non-interactive shell can resolve the
// bridge executable, without running it. The ordinary info probe then checks
// that executable, its configured socket, and the runner protocol together.
// No host keys, control sockets, or peer configuration are written by this check.
func InspectSSH(ctx context.Context, peerURL string) error {
	return inspectSSH(ctx, peerURL, func(ctx context.Context, args []string) (string, error) {
		var stderr diagnosticOutput
		cmd := exec.CommandContext(ctx, "ssh", args...)
		cmd.Stdout, cmd.Stderr = io.Discard, &stderr
		cmd.WaitDelay = 250 * time.Millisecond
		err := cmd.Run()
		return strings.TrimSpace(string(stderr)), err
	})
}

func inspectSSH(ctx context.Context, peerURL string, run func(context.Context, []string) (string, error)) error {
	endpoint := sshEndpointForPeer(peerURL)
	if endpoint.target == "" {
		return fmt.Errorf("invalid SSH peer")
	}
	controlDir, err := sshControlDirPath()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	args := []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "ControlMaster=no", "-o", "ControlPath=" + filepath.Join(controlDir, "%C"), "--", endpoint.target,
		"command -v " + shellQuote(endpoint.command) + " >/dev/null || exit 127"}
	detail, err := run(ctx, args)
	if err == nil {
		return nil
	}
	var exit *exec.ExitError
	missing := errors.As(err, &exit) && exit.ExitCode() == 127
	message := "Non-interactive SSH failed: " + err.Error()
	if missing {
		message = "SSH connected, but its shell could not resolve remote_command " + fmt.Sprintf("%q", endpoint.command)
	}
	if ctx.Err() != nil {
		message = "Non-interactive SSH check timed out or was cancelled"
	}
	if detail != "" {
		message += ": " + detail
	}
	return &SSHDiagnosticError{CommandUnavailable: missing, Detail: message}
}

// SSH banners and errors are untrusted and must not grow diagnostic memory
// without bound. Writes after the limit are consumed and discarded.
type diagnosticOutput []byte

func (b *diagnosticOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 4096 - len(*b); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		*b = append(*b, p...)
	}
	return n, nil
}
