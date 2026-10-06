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
	// The key is OpenSSH's own format, which ssh-keygen reads, when it is
	// installed; errand itself does not need it.
	if _, err := exec.LookPath("ssh-keygen"); err == nil {
		derived, err := exec.Command("ssh-keygen", "-y", "-f", path).Output()
		if err != nil || !strings.HasPrefix(publics[0], strings.TrimSpace(string(derived))) {
			t.Fatalf("private key does not match: %q %v", derived, err)
		}
	}
	if info, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode: %v %v", info.Mode(), err)
	}
	if again, err := EnsureSSHKey(context.Background(), path, "errand"); err != nil || again != publics[0] {
		t.Fatalf("existing key not reused: %q %v", again, err)
	}
	// Only the pair is left, in its own directory, whichever process won.
	for dir, want := range map[string]int{filepath.Dir(path): 2, filepath.Dir(filepath.Dir(path)): 1} {
		if entries, _ := os.ReadDir(dir); len(entries) != want {
			t.Fatalf("%s holds %v", dir, entries)
		}
	}
	// A pair someone broke is reported, not half replaced.
	os.Remove(path + ".pub")
	if _, err := EnsureSSHKey(context.Background(), path, "errand"); err == nil {
		t.Fatal("a key without its public half was used")
	}
}

// A client that never reaches a machine over SSH needs no OpenSSH to make
// the key it sends with lease requests.
func TestEnsureSSHKeyWithoutOpenSSH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	path := filepath.Join(t.TempDir(), "key", "errand_ed25519")
	public, err := EnsureSSHKey(context.Background(), path, "errand")
	if err != nil || !strings.HasPrefix(public, "ssh-ed25519 ") || !strings.HasSuffix(public, " errand") {
		t.Fatalf("key %q: %v", public, err)
	}
}
