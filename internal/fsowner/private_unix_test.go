//go:build unix

package fsowner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateReadsModeBits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for mode, want := range map[os.FileMode]bool{0o600: true, 0o400: true, 0o640: false, 0o604: false, 0o620: false} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Private(f)
		f.Close()
		if err != nil || got != want {
			t.Errorf("mode %v: private %v, %v; want %v", mode, got, err, want)
		}
	}
}
