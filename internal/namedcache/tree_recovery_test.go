package namedcache

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestMissingPublishedGenerationRestoresColdAndGCReclaimsOrphan(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 16)
	key, id := Key{"owner", "project", "deps"}, proto.NewULID()
	if _, err := s.AcquireTree(t.Context(), key, id); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(s.dir, key.hash())
	orphan := "generation-" + proto.NewULID()
	if err := os.MkdirAll(filepath.Join(entry, "trees", orphan, "tree"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "trees", orphan, "tree/payload"), make([]byte, 64), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("trees/generation-"+proto.NewULID(), filepath.Join(entry, "tree-current")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreTree(t.Context(), key, id, t.TempDir(), "cache"); err != nil {
		t.Fatalf("missing generation was not treated as cold: %v", err)
	}
	if _, err := s.GC(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(entry, "trees", orphan)); err != nil {
		t.Fatal("GC touched held tree", err)
	}
	if err := s.ReleaseTree(t.Context(), key, id); err != nil {
		t.Fatal(err)
	}
	preview, err := s.GC(t.Context(), true)
	if err != nil || preview.ReclaimedTemps != 1 || preview.Removed != 0 {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	if _, err := os.Stat(filepath.Join(entry, "trees", orphan)); err != nil {
		t.Fatal("dry run removed orphan", err)
	}
	result, err := s.GC(t.Context(), false)
	if err != nil || result.ReclaimedTemps != 1 || result.Removed != 0 {
		t.Fatalf("GC: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(entry, "trees", orphan)); !os.IsNotExist(err) {
		t.Fatal("orphan retained", err)
	}
}

func TestRestoreCheckpointPrecedesExposureAndCleanupIsHolderScoped(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1<<20)
	key, id := Key{"owner", "project", "deps"}, proto.NewULID()
	if _, err := s.AcquireTree(t.Context(), key, id); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	called := false
	_, err := s.RestoreTree(t.Context(), key, id, workspace, "cache", func(base string) error {
		called = true
		if !ValidTreeBaseline(base) {
			t.Fatal("invalid checkpoint")
		}
		if _, err := os.Stat(filepath.Join(workspace, "cache")); !os.IsNotExist(err) {
			t.Fatal("directory exposed before comparison receipt")
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("checkpoint: %v %v", called, err)
	}
	// Simulate the owned staging directory left by an interrupted restore.
	own := filepath.Join(workspace, restoreStage(key, id, "cache"))
	sibling := filepath.Join(workspace, restoreStage(key, proto.NewULID(), "cache"))
	for _, path := range []string{own, sibling} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DiscardRestore(t.Context(), key, id, workspace, "cache"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Fatal("owned stage retained", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatal("sibling stage removed", err)
	}
}

func TestDiscardRestoreRejectsSymlinkParent(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1<<20)
	key, id := Key{"owner", "project", "deps"}, proto.NewULID()
	workspace, outside := t.TempDir(), t.TempDir()
	stage := filepath.Join(outside, filepath.Base(restoreStage(key, id, "parent/cache")))
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "parent")); err != nil {
		t.Fatal(err)
	}
	if err := s.DiscardRestore(t.Context(), key, id, workspace, "parent/cache"); err == nil {
		t.Fatal("accepted symlink parent")
	}
	if _, err := os.Lstat(stage); err != nil {
		t.Fatalf("cleanup touched another directory through the symlink: %v", err)
	}
}
