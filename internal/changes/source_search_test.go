//go:build darwin || linux

package changes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchSourceRejectsSymlinkTraversalAndRebinding(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "parent"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "parent/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "parent"), 0100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(dir, "parent"), 0700); os.Chmod(filepath.Join(dir, "old"), 0700) })
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{"parent/link/file", "parent/link", "../file"} {
		file, err := openSearchSourceFile(root, name)
		if file != nil {
			file.Close()
		}
		if err == nil {
			t.Fatalf("accepted unsafe path %q", name)
		}
	}
	// The traversal checks above require search-only permissions. Restore write
	// access for the rebinding fixture: macOS may reject renaming a 0100 directory.
	if err := os.Chmod(filepath.Join(dir, "parent"), 0700); err != nil {
		t.Fatal(err)
	}
	err = withSearchSourceParent(root, "parent/file", func(*os.File, string) error {
		if err := os.Rename(filepath.Join(dir, "parent"), filepath.Join(dir, "old")); err != nil {
			t.Fatalf("setting up parent rebinding: %v", err)
		}
		if err := os.Mkdir(filepath.Join(dir, "parent"), 0100); err != nil {
			t.Fatalf("setting up replacement parent: %v", err)
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("accepted rebound parent: %v", err)
	}
}
