//go:build windows

package changes

import (
	"os"
	"path/filepath"
	"testing"
)

// Windows refuses to delete a directory while a handle to it is open, so
// removal must release the tree's root before deleting it.
func TestRemoveTreeDeletesItsRootOnWindows(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".change-base-partial")
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "readonly"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTree(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("tree root survived removal: %v", err)
	}
}
