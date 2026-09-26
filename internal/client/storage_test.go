package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/proto"
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

func TestChangeStorageStatsCancelsWhileWaitingToWidenStaging(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads restrictive staging without widening it")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	key := localChangeKey("http://runner.test", proto.NewULID())
	sealed := filepath.Join(root, "downloads", key, "sealed")
	if err := os.MkdirAll(sealed, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealed, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(sealed, 0o700)
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

func TestTransferInventoryCountsBytesAsTransferGCDoes(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "data"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	prep := prepareSnapshot(root, true, false)
	if prep.err != nil {
		t.Fatal(prep.err)
	}
	opts := RunOptions{PeerURL: "http://runner", Root: root}
	const id = "01M2280R0T4152A3BSV4C2976R"
	if err := recordWorkspaceOrigin(opts, id, prep.manifest); err != nil {
		t.Fatal(err)
	}
	dir, err := workspaceTransferDir(opts.PeerURL, id)
	if err != nil {
		t.Fatal(err)
	}
	want, err := changeops.TransferStorageBytes(dir)
	if err != nil || want == 0 {
		t.Fatalf("transfer bytes %d %v", want, err)
	}
	stats, err := workspaceTransferStats(t.Context())
	if err != nil || stats.Items != 1 || stats.Bytes != want {
		t.Fatalf("inventory %+v %v, transfer GC counts %d", stats, err, want)
	}
}
