package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lydakis/errand/internal/nowindow"
)

// EnsureSSHKey returns the public half of the ed25519 key at path, making
// the key with ssh-keygen first if there is none. errand makes keys of its
// own, without a passphrase, so ssh in batch mode can always use them. The
// private key is the key: it is linked into place whole, and a missing
// public half is derived from it again.
func EnsureSSHKey(ctx context.Context, path, comment string) (string, error) {
	if public, err := os.ReadFile(path + ".pub"); err == nil {
		if _, err := os.Stat(path); err == nil {
			return strings.TrimSpace(string(public)), nil
		}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(dir, ".key-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		made := filepath.Join(tmp, "key")
		if _, err := sshKeygen(ctx, "-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", made); err != nil {
			return "", fmt.Errorf("making an SSH key with ssh-keygen: %v", err)
		}
		// Linking never replaces a key another process made first.
		err := os.Link(made, path)
		if err == nil {
			if err := os.Rename(made+".pub", path+".pub"); err != nil {
				return "", err
			}
			public, err := os.ReadFile(path + ".pub")
			return strings.TrimSpace(string(public)), err
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	// The private key has no public half yet: another process is about to
	// publish it, or one was stopped before it could.
	public, err := sshKeygen(ctx, "-y", "-f", path)
	if err != nil {
		return "", fmt.Errorf("reading SSH key %s: %v", path, err)
	}
	pub := filepath.Join(tmp, "key.pub")
	if err := os.WriteFile(pub, []byte(public+"\n"), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(pub, path+".pub"); err != nil {
		return "", err
	}
	return public, nil
}

func sshKeygen(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "ssh-keygen", args...)
	nowindow.Hide(cmd)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%v %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
