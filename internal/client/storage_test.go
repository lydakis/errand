package client

import (
	"context"
	"errors"
	"github.com/lydakis/errand/internal/proto"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestChangeStatsReportsLocalStateAndDownloads(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	stateRoot := filepath.Join(root, "errand")
	if err := os.MkdirAll(filepath.Join(stateRoot, "jobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateRoot, "downloads", "orphan"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateRoot, "jobs", "record.json"), []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateRoot, "downloads", "orphan", "change"), []byte("download"), 0o600); err != nil {
		t.Fatal(err)
	}

	stats, err := ChangeStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Items != 2 || stats.Bytes < int64(len("state")+len("download")) {
		t.Fatalf("change stats = %+v", stats)
	}
}

func TestChangeStatsKeepsKnownUsageWhenAnotherCandidateDisappears(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stats, err := changeStatsWithCollector(func(_, _ string, candidates map[string]*localChangeCandidate) error {
		candidates["kept"] = &localChangeCandidate{bytes: 42}
		return &os.PathError{Op: "lstat", Path: "vanished", Err: os.ErrNotExist}
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Items != 1 || stats.Bytes != 42 {
		t.Fatalf("change stats = %+v", stats)
	}
}

func TestChangeStorageStatsCancelsWhileTransferIsLocked(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	key := localChangeKey("http://runner.test", proto.NewULID())
	if err := os.MkdirAll(filepath.Join(root, "downloads", key), 0700); err != nil {
		t.Fatal(err)
	}
	unlock, err := acquireLocalChangeLock(localChangeTransferLockName(key))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := ChangeStorageStats(ctx); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("scan error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("storage request ignored cancellation while waiting for a transfer")
	}
}
