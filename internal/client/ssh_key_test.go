package client

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestEnsureSSHKey(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	path := filepath.Join(t.TempDir(), "keys", "errand_ed25519")
	// Processes that make the key at once all end up with the same one.
	publics := make([]string, 4)
	var wg sync.WaitGroup
	for i := range publics {
		wg.Go(func() {
			public, err := EnsureSSHKey(context.Background(), path, "errand")
			if err != nil {
				t.Error(err)
			}
			publics[i] = public
		})
	}
	wg.Wait()
	for _, public := range publics {
		if !strings.HasPrefix(public, "ssh-ed25519 ") || public != publics[0] {
			t.Fatalf("public keys %q", publics)
		}
	}
	derived, err := exec.Command("ssh-keygen", "-y", "-f", path).Output()
	if err != nil || !strings.HasPrefix(publics[0], strings.TrimSpace(string(derived))) {
		t.Fatalf("private key does not match: %q %v", derived, err)
	}
	if info, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode: %v %v", info.Mode(), err)
	}
	if again, err := EnsureSSHKey(context.Background(), path, "errand"); err != nil || again != publics[0] {
		t.Fatalf("existing key not reused: %q %v", again, err)
	}
	// A process stopped between placing the two halves leaves only the
	// private key; the public half is derived from it again.
	os.Remove(path + ".pub")
	if again, err := EnsureSSHKey(context.Background(), path, "errand"); err != nil || again != publics[0] {
		t.Fatalf("public half not restored: %q %v", again, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 2 {
		t.Fatalf("left behind %v", entries)
	}
}
