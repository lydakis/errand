package snapshot

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Manual invalidations isolate correctness from the native backend's delivery
// timing. Directory and policy changes must be detected without event hints.
func prepareWatchFixture(t *testing.T) (*Watch, *Builder) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "value"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	return &Watch{root: root, identity: info, Changed: make(chan struct{}, 1)}, new(Builder)
}

func assertPreparedMatchesFull(t *testing.T, w *Watch, b *Builder) *SelectionGuard {
	t.Helper()
	got, _, _, guard, err := w.Prepare(b)
	if err != nil {
		t.Fatal(err)
	}
	paths, _, _, err := SelectFilesWithOptions(w.root, w.opts)
	if err != nil {
		t.Fatal(err)
	}
	want, err := Build(w.root, paths)
	if err != nil {
		t.Fatal(err)
	}
	if got.RootHash() != want.RootHash() {
		t.Fatalf("incremental source differs from a fresh snapshot: %+v vs %+v", got, want)
	}
	if err := guard.Verify(); err != nil {
		t.Fatal(err)
	}
	return guard
}

func TestWatchPrepareRehashesDirtyContentAndKeepsPriorEntries(t *testing.T) {
	w, b := prepareWatchFixture(t)
	assertPreparedMatchesFull(t, w, b)
	if err := os.WriteFile(filepath.Join(w.root, "value"), []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	w.invalidatePath(filepath.Join(w.root, "value"), false)
	assertPreparedMatchesFull(t, w, b)
}

func TestWatchPrepareInvalidatesHashesEvenWhenStatEvidenceMatches(t *testing.T) {
	for _, reconcile := range []bool{false, true} {
		t.Run(map[bool]string{false: "dirty-file", true: "reconciliation"}[reconcile], func(t *testing.T) {
			w, b := prepareWatchFixture(t)
			assertPreparedMatchesFull(t, w, b)
			name := filepath.Join(w.root, "value")
			if err := os.WriteFile(name, []byte("after!"), 0600); err != nil {
				t.Fatal(err)
			}
			// Simulate a filesystem returning identical stat evidence for two
			// same-size writes, without relying on the host's timestamp resolution.
			old := b.hashes[name]
			old.info, _ = os.Lstat(name)
			old.seconds, old.nanos, _ = changeStamp(old.info)
			b.hashes[name] = old
			if reconcile {
				w.InvalidatePreparation()
			} else {
				w.invalidatePath(name, false)
			}
			assertPreparedMatchesFull(t, w, b)
		})
	}
}

func TestWatchPrepareChecksStructuralChangesWithoutEvents(t *testing.T) {
	w, b := prepareWatchFixture(t)
	guard := assertPreparedMatchesFull(t, w, b)
	if err := os.Mkdir(filepath.Join(w.root, "new"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.root, "new", "file"), []byte("added"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := guard.Verify(); err == nil {
		t.Fatal("guard accepted changed selection before event delivery")
	}
	assertPreparedMatchesFull(t, w, b)
	if err := os.Remove(filepath.Join(w.root, "value")); err != nil {
		t.Fatal(err)
	}
	assertPreparedMatchesFull(t, w, b)
}

func TestWatchPreparePolicyChangesNeverAuthorizeExcludedFiles(t *testing.T) {
	w, b := prepareWatchFixture(t)
	if err := os.WriteFile(filepath.Join(w.root, "secret"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	guard := assertPreparedMatchesFull(t, w, b)
	if err := os.WriteFile(filepath.Join(w.root, ".errandignore"), []byte("secret\nvalue\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := guard.Verify(); err == nil {
		t.Fatal("guard accepted changed policy before event delivery")
	}
	// A dirty event only identifies work; it cannot select an excluded source.
	w.invalidatePath(filepath.Join(w.root, "secret"), false)
	assertPreparedMatchesFull(t, w, b)
}

func TestWatchPrepareOverflowReconcilesWithoutDirtyPaths(t *testing.T) {
	w, b := prepareWatchFixture(t)
	assertPreparedMatchesFull(t, w, b)
	if err := os.WriteFile(filepath.Join(w.root, "value"), []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	w.invalidate()
	assertPreparedMatchesFull(t, w, b)
}

func TestWatchPrepareRejectsReplacedRoot(t *testing.T) {
	w, b := prepareWatchFixture(t)
	guard := assertPreparedMatchesFull(t, w, b)
	old := w.root + "-old"
	if err := os.Rename(w.root, old); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(old)
	if err := os.Mkdir(w.root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := guard.Verify(); err == nil {
		t.Fatal("guard accepted a replacement checkout")
	}
	if _, _, _, _, err := w.Prepare(b); err == nil {
		t.Fatal("prepared a replacement checkout")
	}
}

func TestWatchPrepareRetainsDirtyWorkAfterReadFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses unreadable-file permissions")
	}
	w, b := prepareWatchFixture(t)
	assertPreparedMatchesFull(t, w, b)
	name := filepath.Join(w.root, "value")
	if err := os.WriteFile(name, []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(name, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(name, 0600)
	w.invalidatePath(name, false)
	if _, _, _, _, err := w.Prepare(b); err == nil {
		t.Fatal("snapshot accepted unreadable changed content")
	}
	if err := os.Chmod(name, 0600); err != nil {
		t.Fatal(err)
	}
	// No second hint: the failed batch must not discard its pending work.
	assertPreparedMatchesFull(t, w, b)
}

func TestWatchPrepareConsumesNativeDirtyNotifications(t *testing.T) {
	stub, b := prepareWatchFixture(t)
	w, err := WatchFiles(stub.root, stub.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	assertPreparedMatchesFull(t, w, b)
	before := w.Generation()
	if err := os.WriteFile(filepath.Join(w.root, "value"), []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for w.Generation() <= before {
		select {
		case <-w.Changed:
		case err := <-w.Errors:
			t.Fatal(err)
		case <-timer.C:
			t.Fatal("source write was not observed")
		}
	}
	assertPreparedMatchesFull(t, w, b)
}

func TestWatchInvalidatePreparationResamplesWithoutScheduling(t *testing.T) {
	w, b := prepareWatchFixture(t)
	assertPreparedMatchesFull(t, w, b)
	name := filepath.Join(w.root, "value")
	if err := os.WriteFile(name, []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	// Freeze detected this write before its native event arrived. Request a full
	// source refresh without shortening the caller's existing retry backoff.
	before := w.Generation()
	w.InvalidatePreparation()
	if w.Generation() != before {
		t.Fatal("preparation invalidation advanced native generation")
	}
	select {
	case <-w.Changed:
		t.Fatal("preparation invalidation scheduled an extra cycle")
	default:
	}
	assertPreparedMatchesFull(t, w, b)
	// A queued native notification must survive a later preparation invalidation.
	if err := os.WriteFile(name, []byte("latest"), 0600); err != nil {
		t.Fatal(err)
	}
	w.invalidatePath(name, false)
	before = w.Generation()
	w.InvalidatePreparation()
	if w.Generation() != before {
		t.Fatal("preparation invalidation advanced native generation")
	}
	select {
	case <-w.Changed:
	default:
		t.Fatal("queued native notification was discarded")
	}
	assertPreparedMatchesFull(t, w, b)
}
