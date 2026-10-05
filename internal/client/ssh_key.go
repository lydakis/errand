package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/nowindow"
)

// EnsureSSHKey returns the public half of the ed25519 key at path, making
// the key with ssh-keygen first if there is none. errand makes keys of its
// own, without a passphrase, so ssh in batch mode can always use them.
func EnsureSSHKey(ctx context.Context, path, comment string) (string, error) {
	if public, ok := readSSHKey(path); ok {
		return public, nil
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
	made := filepath.Join(tmp, "key")
	cmd := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", made)
	nowindow.Hide(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("making an SSH key with ssh-keygen: %v %s", err, strings.TrimSpace(string(out)))
	}
	// Linking never replaces a key another process made first; the public
	// half goes last, since a key counts as made once it is there.
	if err := os.Link(made, path); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	} else if err == nil {
		if err := os.Link(made+".pub", path+".pub"); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		if public, ok := readSSHKey(path); ok {
			return public, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("SSH key %s has no public half %s.pub; remove it to make a new one", path, path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readSSHKey(path string) (string, bool) {
	public, err := os.ReadFile(path + ".pub")
	if err != nil {
		return "", false
	}
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	return strings.TrimSpace(string(public)), true
}
