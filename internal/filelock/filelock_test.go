package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func openTwice(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lock")
	first, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	second, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	return first, second
}

func TestExclusiveLockExcludesOtherOpenFiles(t *testing.T) {
	first, second := openTwice(t)
	if err := TryLock(first); err != nil {
		t.Fatal(err)
	}
	if err := TryLock(second); !errors.Is(err, ErrLocked) {
		t.Fatalf("second exclusive lock: %v, want ErrLocked", err)
	}
	if err := TryRLock(second); !errors.Is(err, ErrLocked) {
		t.Fatalf("shared lock under exclusive: %v, want ErrLocked", err)
	}
	if err := Unlock(first); err != nil {
		t.Fatal(err)
	}
	if err := TryLock(second); err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
}

func TestSharedLocksCoexistButExcludeExclusive(t *testing.T) {
	first, second := openTwice(t)
	if err := TryRLock(first); err != nil {
		t.Fatal(err)
	}
	if err := TryRLock(second); err != nil {
		t.Fatalf("second shared lock: %v", err)
	}
	if err := Unlock(second); err != nil {
		t.Fatal(err)
	}
	if err := TryLock(second); !errors.Is(err, ErrLocked) {
		t.Fatalf("exclusive lock under shared: %v, want ErrLocked", err)
	}
}
