package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCacheProjectIdentitySurvivesEditsAndRename(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	first, err := cacheProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source"), []byte("edited"), 0600); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "renamed")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	next, err := cacheProjectID(moved)
	if err != nil || next != first {
		t.Fatalf("changed identity: %q %q %v", first, next, err)
	}
	other, err := cacheProjectID(t.TempDir())
	if err != nil || other == first {
		t.Fatalf("shared checkout identity: %q %v", other, err)
	}
}
