package durable

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSyncFilesAndDirectories(t *testing.T) {
	dir := t.TempDir()
	file, err := os.Create(filepath.Join(dir, "file"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("data"); err != nil {
		t.Fatal(err)
	}
	if err := Sync(file); err != nil {
		t.Fatalf("sync file: %v", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := Sync(directory); err != nil {
		t.Fatalf("sync directory: %v", err)
	}
}
