package changes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestSourceDeltaValidationPrecedesExpansionAndHonorsContext(t *testing.T) {
	base := observationManifest(map[string]string{"a": "before"})
	current := observationManifest(map[string]string{"a": "after"})
	delta, err := PrepareSourceDelta(t.Context(), base, current, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	malformed := delta
	malformed.V++
	malformed.BaselineRoot = strings.Repeat("0", 64)
	if _, err := ExpandSourceDeltaContext(t.Context(), base, malformed, current.RootHash(), 1<<20); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("shape was not rejected first: %v", err)
	}
	malformed = delta
	malformed.Paths = []string{"a", "a"}
	if _, err := ExpandSourceDeltaContext(t.Context(), base, malformed, current.RootHash(), 1<<20); err == nil || !strings.Contains(err.Error(), "sorted") {
		t.Fatalf("duplicate roots: %v", err)
	}
	malformed = delta
	malformed.Paths = make([]string, MaxChangeEntries+1)
	if _, err := ExpandSourceDeltaContext(t.Context(), base, malformed, current.RootHash(), 1<<20); !errors.Is(err, ErrEntryLimitExceeded) {
		t.Fatalf("root count limit: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ExpandSourceDeltaContext(ctx, base, delta, current.RootHash(), 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("expansion: %v", err)
	}
	if _, err := AcceptedSourceContext(ctx, base, current, []string{"a"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("acceptance: %v", err)
	}
	if err := ValidateBundleContext(ctx, delta); !errors.Is(err, context.Canceled) {
		t.Fatalf("validation: %v", err)
	}
}

func TestPreparedSourceOwnsMetadataAndBindsStage(t *testing.T) {
	root, b, staged := applyFixture(t, "before\n", "after\n")
	target := transferTarget(t, root)
	s := TransferSession{Directory: t.TempDir(), Root: root, RootID: target.RootID, Owner: "owner", SourceID: "source", MaxSourceBytes: 1 << 20, MaxChangeBytes: 1 << 20}
	if err := s.Initialize(t.Context(), filepath.Join(staged, "base"), b.BaseManifest); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareTransferSource(t.Context(), b.BaseManifest, b.RemoteManifest, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	originalRoot := b.RemoteManifest.RootHash()
	b.RemoteManifest.Entries[0].SHA256 = strings.Repeat("0", 64)
	exposed := prepared.Manifest()
	exposed.Entries[0].SHA256 = strings.Repeat("1", 64)
	delta := prepared.Delta()
	delta.Paths[0] = "elsewhere"
	delta.RemoteManifest.Entries[0].Path = "elsewhere"
	if prepared.Manifest().RootHash() != originalRoot {
		t.Fatal("caller mutated prepared metadata")
	}
	id := proto.NewULID()
	dir, _, err := s.StagePrepared(t.Context(), id, filepath.Join(staged, "remote"), prepared)
	if err != nil {
		t.Fatal(err)
	}
	staleID := proto.NewULID()
	if _, _, err := s.StagePrepared(t.Context(), staleID, filepath.Join(staged, "remote"), prepared); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(id, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.StagePrepared(t.Context(), staleID, "unused", prepared); !errors.Is(err, ErrCheckpointChanged) {
		t.Fatalf("reused unstarted stage missed checkpoint drift: %v", err)
	}
	if _, _, err := s.StagePrepared(t.Context(), proto.NewULID(), filepath.Join(staged, "remote"), prepared); !errors.Is(err, ErrCheckpointChanged) {
		t.Fatalf("stale prepared source: %v", err)
	}
	s.MaxSourceBytes, s.MaxChangeBytes = 0, 0
	if _, _, err := s.StagePrepared(t.Context(), id, "missing-source", prepared); err != nil {
		t.Fatalf("exact replay lost: %v", err)
	}
	s.MaxSourceBytes, s.MaxChangeBytes = 1<<20, 1<<20
	if err := os.WriteFile(filepath.Join(dir, "remote", "artifact"), []byte("corrupt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.StagePrepared(t.Context(), id, "missing-source", prepared); err == nil {
		t.Fatal("replay accepted corrupt staging")
	}
}

func TestPreparedSourceRejectsCorruptBodyAndFullSourceQuota(t *testing.T) {
	root, b, staged := applyFixture(t, "before\n", "after\n")
	target := transferTarget(t, root)
	s := TransferSession{Directory: t.TempDir(), Root: root, RootID: target.RootID, Owner: "owner", SourceID: "source", MaxSourceBytes: 1 << 20, MaxChangeBytes: 1 << 20}
	if err := s.Initialize(t.Context(), filepath.Join(staged, "base"), b.BaseManifest); err != nil {
		t.Fatal(err)
	}
	prepared, err := ExpandTransferSource(t.Context(), b.BaseManifest, b, b.RemoteManifest.RootHash(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	b.Paths[0] = "caller-changed-root"
	s.MaxSourceBytes = 1
	if _, _, err := s.StagePrepared(t.Context(), proto.NewULID(), filepath.Join(staged, "remote"), prepared); !errors.Is(err, ErrByteLimitExceeded) {
		t.Fatalf("full source quota: %v", err)
	}
	s.MaxSourceBytes = 1 << 20
	if err := os.WriteFile(filepath.Join(staged, "remote", "artifact"), []byte("broken\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, rejected, err := s.StagePrepared(t.Context(), proto.NewULID(), filepath.Join(staged, "remote"), prepared)
	if err == nil {
		t.Fatal("accepted corrupt source body")
	}
	rejected.Paths[0] = "mutated-return-value"
	if prepared.Delta().Paths[0] != "artifact" {
		t.Fatal("failed stage exposed mutable prepared delta")
	}

}

func TestPreparedSourceStagesSparseBodies(t *testing.T) {
	root, source := t.TempDir(), t.TempDir()
	initial := observationManifest(map[string]string{"edit": "before\n", "unchanged": "retained\n"})
	current := observationManifest(map[string]string{"edit": "after\n", "unchanged": "retained\n"})
	writeTransferFile(t, root, "edit", "before\n")
	writeTransferFile(t, root, "unchanged", "retained\n")
	writeTransferFile(t, source, "edit", "after\n")
	target := transferTarget(t, root)
	s := TransferSession{Directory: t.TempDir(), Root: root, RootID: target.RootID, Owner: "owner", SourceID: "source", MaxSourceBytes: 1 << 20, MaxChangeBytes: 1 << 20}
	if err := s.Initialize(t.Context(), root, initial); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareTransferSource(t.Context(), initial, current, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	id := proto.NewULID()
	if _, _, err := s.StagePrepared(t.Context(), id, source, prepared); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(id, nil, false); err != nil {
		t.Fatal(err)
	}
	assertTransferFile(t, root, "edit", "after\n")
	assertTransferFile(t, root, "unchanged", "retained\n")
	checkpoint, err := s.Checkpoint().Read()
	if err != nil || checkpoint.Manifest.RootHash() != current.RootHash() {
		t.Fatalf("checkpoint=%+v err=%v", checkpoint, err)
	}
}
