package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOutputStatsReportsLocalStateAndDownloads(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(stateRoot, "downloads", "orphan", "output"), []byte("download"), 0o600); err != nil {
		t.Fatal(err)
	}

	stats, err := OutputStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Items != 2 || stats.Bytes < int64(len("state")+len("download")) {
		t.Fatalf("output stats = %+v", stats)
	}
}

func TestOutputStatsKeepsKnownUsageWhenAnotherCandidateDisappears(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stats, err := outputStatsWithCollector(func(_, _ string, candidates map[string]*localOutputCandidate) error {
		candidates["kept"] = &localOutputCandidate{bytes: 42}
		return &os.PathError{Op: "lstat", Path: "vanished", Err: os.ErrNotExist}
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Items != 1 || stats.Bytes != 42 {
		t.Fatalf("output stats = %+v", stats)
	}
}
