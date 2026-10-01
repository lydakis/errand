package fsowner

import (
	"os"
	"testing"
)

func TestNewDirectoryIsOwnedByCurrentUser(t *testing.T) {
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	owned, err := OwnedByCurrentUser(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !owned {
		t.Fatal("a directory this process created is not owned by the current user")
	}
}
