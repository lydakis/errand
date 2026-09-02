package client

import (
	"os"
	"path/filepath"
	"testing"
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
