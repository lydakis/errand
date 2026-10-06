//go:build windows

package fsowner

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A file in the user's profile is private; one whose DACL also lets
// BUILTIN\Users read it is not.
func TestPrivateReadsTheDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := isPrivate(t, path); !got {
		t.Fatal("a new file in the user's temporary directory is not private")
	}
	if out, err := exec.Command("icacls", path, "/grant", "*S-1-5-32-545:(R)").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v %s", err, out)
	}
	if got := isPrivate(t, path); got {
		t.Fatal("a file BUILTIN\\Users can read is private")
	}
}

func isPrivate(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	private, err := Private(f)
	if err != nil {
		t.Fatal(err)
	}
	return private
}
