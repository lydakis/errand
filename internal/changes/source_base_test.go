package changes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

// advancedSession applies one transfer, so its checkpoint record is no longer
// hashed by validation and its identity is computed only on demand.
func advancedSession(t *testing.T) (*TransferSession, proto.ChangeBundle) {
	t.Helper()
	root, b, staged := applyFixture(t, "original\n", "source\n")
	target := transferTarget(t, root)
	s := &TransferSession{Directory: t.TempDir(), Root: root, RootID: target.RootID, Owner: "owner", SourceID: "sender",
		MaxSourceBytes: 1 << 20, MaxChangeBytes: 1 << 20, Reuse: NewCheckpointCache(4, 1<<20)}
	if err := s.Initialize(t.Context(), filepath.Join(staged, "base"), b.BaseManifest); err != nil {
		t.Fatal(err)
	}
	id := proto.NewULID()
	if _, _, err := s.Stage(t.Context(), id, filepath.Join(staged, "remote"), b.RemoteManifest); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(id, nil, false); err != nil {
		t.Fatal(err)
	}
	return s, b
}

// nextSource changes the checkpointed artifact's metadata. Expansion and the
// checkpoint checks before staging need no bodies.
func nextSource(t *testing.T, base proto.Manifest) (proto.Manifest, proto.ChangeBundle) {
	t.Helper()
	next := cloneSourceManifest(base)
	next.Entries[0].SHA256, next.Entries[0].Size = strings.Repeat("a", 64), 5
	delta, err := PrepareSourceDelta(t.Context(), base, next, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return next, delta
}

func TestSourceBaseIdentityDescribesOnlyItsOwnCopy(t *testing.T) {
	m := observationManifest(map[string]string{"a": "one", "b": "two"})
	want := m.RootHash()
	b := NewSourceBase(m)
	// Neither the caller's input nor a returned copy can change the entries the
	// identity is computed from, before or after it is computed.
	m.Entries[0].SHA256 = strings.Repeat("0", 64)
	exposed := b.Manifest()
	exposed.Entries[1].SHA256 = strings.Repeat("1", 64)
	if err := b.validate(); err != nil {
		t.Fatal(err)
	}
	root := b.rootHash()
	if root != want {
		t.Fatalf("identity = %q; want %q", root, want)
	}
	exposed = b.Manifest()
	exposed.Entries[0].Path = "changed"
	if b.Manifest().RootHash() != root {
		t.Fatal("retained entries no longer match their identity")
	}
}

func TestSourceBaseRetainsCheckpointValidation(t *testing.T) {
	s, _ := advancedSession(t)
	reserved := NewSourceBase(observationManifest(map[string]string{".git/config": "body"}))
	for range 2 {
		if err := s.InitializeBase(t.Context(), "missing", reserved); err == nil || !strings.Contains(err.Error(), "Git metadata") {
			t.Fatalf("invalid creation snapshot: %v", err)
		}
	}
}

func TestRetainedCreationBaseIsComparedWithEveryCheckpointRead(t *testing.T) {
	s, b := advancedSession(t)
	creation, other := NewSourceBase(b.BaseManifest), NewSourceBase(b.RemoteManifest)
	if err := s.InitializeBase(t.Context(), "missing", creation); err != nil {
		t.Fatal(err)
	}
	if err := s.InitializeBase(t.Context(), "missing", other); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("different creation snapshot: %v", err)
	}
	// Once a published checkpoint names another creation snapshot, the base whose
	// identity was already accepted is refused and the named one is accepted.
	otherRoot := other.rootHash()
	path := s.Checkpoint().StatePath
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state checkpointState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	state.InitialRoot = otherRoot
	if raw, err = json.Marshal(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.InitializeBase(t.Context(), "missing", creation); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("changed checkpoint accepted the retained creation snapshot: %v", err)
	}
	if err := s.InitializeBase(t.Context(), "missing", other); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw[:len(raw)-1], 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.InitializeBase(t.Context(), "missing", other); err == nil {
		t.Fatal("corrupt checkpoint accepted")
	}
}

func TestCheckpointBaseSharesRecordIdentityOnlyForItsBytes(t *testing.T) {
	s, b := advancedSession(t)
	c := s.Checkpoint()
	base, err := c.Base()
	if err != nil {
		t.Fatal(err)
	}
	root := base.rootHash()
	if root != b.RemoteManifest.RootHash() {
		t.Fatalf("identity = %q", root)
	}
	// A stage that reads the same bytes finds the identity on the shared record.
	if record := s.Reuse.get(c.StatePath); record == nil || record.root != root {
		t.Fatal("checkpoint base did not share its record's identity")
	}
	next, delta := nextSource(t, b.RemoteManifest)
	prepared, err := ExpandTransferSourceBase(t.Context(), base, delta, next.RootHash(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	// Same size and timestamps, different bytes: the record is decoded again
	// and gets its own identity, so the earlier one cannot vouch for it.
	info, err := os.Stat(c.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(c.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	digest := b.RemoteManifest.Entries[0].SHA256
	changed := bytes.Replace(raw, []byte(digest), []byte(strings.Repeat("c", len(digest))), 1)
	if bytes.Equal(raw, changed) {
		t.Fatal("fixture did not change the checkpoint")
	}
	if err := os.WriteFile(c.StatePath, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(c.StatePath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	rebased, err := c.Base()
	if err != nil {
		t.Fatal(err)
	}
	if got := rebased.rootHash(); got == root || got != rebased.Manifest().RootHash() {
		t.Fatalf("changed checkpoint identity = %q", got)
	}
	if _, err := ExpandTransferSourceBase(t.Context(), rebased, delta, next.RootHash(), 1<<20); !errors.Is(err, ErrCheckpointChanged) {
		t.Fatalf("expanded against a changed checkpoint: %v", err)
	}
	if _, _, err := s.StagePrepared(t.Context(), proto.NewULID(), "unused", prepared); !errors.Is(err, ErrCheckpointChanged) {
		t.Fatalf("staged against a changed checkpoint: %v", err)
	}
	if base.rootHash() != root {
		t.Fatal("earlier base changed its identity")
	}
}

func TestExpansionRejectsBaseOtherThanDeltaBaseline(t *testing.T) {
	base := observationManifest(map[string]string{"a": "before", "b": "same"})
	current := observationManifest(map[string]string{"a": "after", "b": "same"})
	delta, err := PrepareSourceDelta(t.Context(), base, current, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	// The changed root's merge base matches, so only the base identity differs.
	other := observationManifest(map[string]string{"a": "before", "b": "other"})
	if _, err := ExpandTransferSourceBase(t.Context(), NewSourceBase(other), delta, current.RootHash(), 1<<20); !errors.Is(err, ErrCheckpointChanged) {
		t.Fatalf("expanded against another base: %v", err)
	}
	prepared, err := ExpandTransferSourceBase(t.Context(), NewSourceBase(base), delta, current.RootHash(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Delta().BaselineRoot != base.RootHash() || prepared.Manifest().RootHash() != current.RootHash() {
		t.Fatal("expansion changed the delta or source identity")
	}
}

// Expansion runs outside the workspace lock with a base shared with requests
// that hold it; the race detector checks the shared record and identity.
func TestSharedSourceBasesAcrossConcurrentRequests(t *testing.T) {
	s, b := advancedSession(t)
	base, err := s.Checkpoint().Base()
	if err != nil {
		t.Fatal(err)
	}
	creation := NewSourceBase(b.BaseManifest)
	next, delta := nextSource(t, b.RemoteManifest)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := ExpandTransferSourceBase(context.Background(), base, delta, next.RootHash(), 1<<20); err != nil {
				t.Error(err)
			}
			if err := creation.validate(); err != nil || creation.rootHash() != b.BaseManifest.RootHash() {
				t.Errorf("creation identity: %v", err)
			}
		})
	}
	for range 4 {
		if err := s.InitializeBase(t.Context(), "missing", creation); err != nil {
			t.Error(err)
		}
		if v, err := s.Checkpoint().readVersion(); err != nil || v.rootHash() != b.RemoteManifest.RootHash() {
			t.Errorf("checkpoint identity: %v", err)
		}
	}
	wg.Wait()
}
