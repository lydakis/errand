package changes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

func TestTransferSessionRecoveryAndCollection(t *testing.T) {
	ctx := context.Background()
	root, b, staged := applyFixture(t, "original\n", "source\n")
	target := transferTarget(t, root)
	s := TransferSession{Directory: t.TempDir(), Root: root, RootID: target.RootID, Owner: "owner", SourceID: "sender", MaxBytes: 1 << 20}
	if err := s.Initialize(ctx, filepath.Join(staged, "base"), b.BaseManifest); err != nil {
		t.Fatal(err)
	}
	id, stale := proto.NewULID(), proto.NewULID()
	dir, delta, err := s.Stage(ctx, id, filepath.Join(staged, "remote"), b.RemoteManifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Stage(ctx, stale, filepath.Join(staged, "remote"), b.RemoteManifest); err != nil {
		t.Fatal(err)
	}
	// Simulate interruption after the durable apply receipt but before checkpoint publication.
	a, err := s.Attempt(id)
	if err != nil {
		t.Fatal(err)
	}
	a.Applying = true
	if err := s.Blobs().Retain(ctx, filepath.Join(dir, "remote"), delta.RemoteManifest); err != nil {
		t.Fatal(err)
	}
	if err := writeTransferJSON(filepath.Join(dir, "attempt.json"), a); err != nil {
		t.Fatal(err)
	}
	target = TransferTarget{Root: root, RootID: s.RootID, Owner: s.Owner, StatePath: filepath.Join(dir, "receipt.json")}
	if _, err := target.Apply(dir, delta, nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	writeTransferFile(t, root, "artifact", "later destination\n")
	preview, err := s.GC(ctx, time.Now().Add(time.Hour), true, []proto.Manifest{b.BaseManifest})
	if err != nil || preview.Protected != 1 {
		t.Fatalf("pending GC: %+v %v", preview, err)
	}
	if err := s.Recover(); err != nil {
		t.Fatal(err)
	}
	assertTransferFile(t, root, "artifact", "later destination\n")
	v, err := s.Checkpoint().Read()
	if err != nil || v.Revision != 1 || v.Manifest.RootHash() != b.RemoteManifest.RootHash() {
		t.Fatalf("checkpoint: %+v %v", v, err)
	}
	if _, err := s.Apply(stale, nil, false); !errors.Is(err, ErrCheckpointChanged) {
		t.Fatalf("stale apply: %v", err)
	}
	if _, err := s.Apply(id, nil, false); err != nil {
		t.Fatal(err)
	}
	assertTransferFile(t, root, "artifact", "later destination\n")
	gc, err := s.GC(ctx, time.Now().Add(time.Hour), false, []proto.Manifest{b.BaseManifest})
	if err != nil || gc.Removed != 1 || gc.Protected != 1 || gc.FreedBytes == 0 {
		t.Fatalf("GC: %+v %v", gc, err)
	}
	if err := s.Blobs().MaterializeBase(ctx, t.TempDir(), v.Manifest, 1<<20); err != nil {
		t.Fatal(err)
	}
}

func TestTransferGCFinishesInterruptedDeletion(t *testing.T) {
	ctx := context.Background()
	root, b, staged := applyFixture(t, "original\n", "source\n")
	target := transferTarget(t, root)
	s := TransferSession{Directory: t.TempDir(), Root: root, RootID: target.RootID, Owner: "owner", SourceID: "sender", MaxBytes: 1 << 20}
	if err := s.Initialize(ctx, filepath.Join(staged, "base"), b.BaseManifest); err != nil {
		t.Fatal(err)
	}
	dir, _, err := s.Stage(ctx, proto.NewULID(), filepath.Join(staged, "remote"), b.RemoteManifest)
	if err != nil {
		t.Fatal(err)
	}
	garbage := filepath.Join(filepath.Dir(dir), ".removing-"+filepath.Base(dir))
	if err := os.Rename(dir, garbage); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(garbage, "attempt.json")); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(); err != nil {
		t.Fatalf("deletion debris blocked recovery: %v", err)
	}
	// Committed deletion must finish even with a cutoff older than the attempt.
	result, err := s.GC(ctx, time.Now().Add(-time.Hour), false, nil)
	if err != nil || result.Removed != 1 {
		t.Fatalf("GC: %+v %v", result, err)
	}
	if _, err := os.Stat(garbage); !os.IsNotExist(err) {
		t.Fatalf("debris remains: %v", err)
	}
}

func TestTransferSessionPartialConflictCheckpoint(t *testing.T) {
	ctx := context.Background()
	root, b, staged := mixedTransferFixture(t)
	target := transferTarget(t, root)
	s := TransferSession{Directory: t.TempDir(), Root: root, RootID: target.RootID, Owner: "owner", SourceID: "sender", MaxBytes: 1 << 20}
	if err := s.Initialize(ctx, filepath.Join(staged, "base"), b.BaseManifest); err != nil {
		t.Fatal(err)
	}
	id := proto.NewULID()
	if _, _, err := s.Stage(ctx, id, filepath.Join(staged, "remote"), b.RemoteManifest); err != nil {
		t.Fatal(err)
	}
	_, err := s.Apply(id, nil, true)
	var conflict *MergeConflictError
	if !errors.As(err, &conflict) || !conflict.Materialized {
		t.Fatalf("conflict: %v", err)
	}
	v, err := s.Checkpoint().Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range conflict.Paths {
		if !sameManifestEntries(subtreeManifest(v.Manifest, p), subtreeManifest(b.BaseManifest, p)) {
			t.Fatalf("conflicted source advanced: %s", p)
		}
	}
	if _, err := s.GC(ctx, time.Now().Add(time.Hour), false, []proto.Manifest{b.BaseManifest}); err != nil {
		t.Fatal(err)
	}
	if err := s.Blobs().MaterializeBase(ctx, t.TempDir(), v.Manifest, 1<<20); err != nil {
		t.Fatal(err)
	}
}
func sameManifestEntries(a, b proto.Manifest) bool { return a.RootHash() == b.RootHash() }

func TestTransferSessionStagePermissionAndIDBinding(t *testing.T) {
	ctx := context.Background()
	root, b, staged := applyFixture(t, "base\n", "remote\n")
	target := transferTarget(t, root)
	s := TransferSession{Directory: t.TempDir(), Root: root, RootID: target.RootID, Owner: "owner", SourceID: "sender", MaxBytes: 1 << 20}
	if err := s.Initialize(ctx, filepath.Join(staged, "base"), b.BaseManifest); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(staged, "remote")
	b.RemoteManifest.Entries[0].Mode = 0
	if err := os.Chmod(filepath.Join(remote, "artifact"), 0); err != nil {
		t.Fatal(err)
	}
	id := proto.NewULID()
	if _, _, err := s.Stage(ctx, id, remote, b.RemoteManifest); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(id, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Stage(ctx, id, filepath.Join(staged, "base"), b.BaseManifest); err == nil {
		t.Fatal("ID reused for a different source")
	}
}

func TestTransferSessionRejectsChangedBaseBeforeApply(t *testing.T) {
	ctx := context.Background()
	root, b, staged := applyFixture(t, "initial\n", "remote\n")
	target := transferTarget(t, root)
	s := TransferSession{Directory: t.TempDir(), Root: root, RootID: target.RootID, Owner: "owner", SourceID: "source", MaxBytes: 1 << 20}
	if err := s.Initialize(ctx, filepath.Join(staged, "base"), b.BaseManifest); err != nil {
		t.Fatal(err)
	}
	id := proto.NewULID()
	dir, delta, err := s.Stage(ctx, id, filepath.Join(staged, "remote"), b.RemoteManifest)
	if err != nil {
		t.Fatal(err)
	}
	// An internally valid replacement archive must still match the checkpoint's
	// actual base contents, not merely repeat the declared root hash.
	wrongRoot, wrong, wrongStage := applyFixture(t, "changed\n", "remote\n")
	_ = wrongRoot
	if err := RemoveTree(filepath.Join(dir, "base")); err != nil {
		t.Fatal(err)
	}
	if err := CopyTransferSource(ctx, filepath.Join(wrongStage, "base"), filepath.Join(dir, "base"), wrong.BaseManifest, 1<<20); err != nil {
		t.Fatal(err)
	}
	delta.BaseManifest = wrong.BaseManifest
	delta.Bytes = wrong.Bytes
	if err := writeTransferJSON(filepath.Join(dir, "bundle.json"), delta); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(id, nil, false); err == nil {
		t.Fatal("accepted forged base")
	}
	assertTransferFile(t, root, "artifact", "initial\n")
}
