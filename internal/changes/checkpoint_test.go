package changes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func checkpointFor(t *testing.T, target TransferTarget) TransferCheckpoint {
	t.Helper()
	return TransferCheckpoint{Root: target.Root, RootID: target.RootID, Owner: target.Owner,
		SourceID: "originating-checkout", StatePath: filepath.Join(t.TempDir(), "checkpoint.json")}
}

func TestCheckpointRecordsSourceAndPreservesDestinationEdits(t *testing.T) {
	base := "first\n" + strings.Repeat("middle\n", 12) + "last\n"
	source := strings.Replace(base, "first", "source", 1)
	root, bundle, staged := applyFixture(t, base, source)
	target := transferTarget(t, root)
	checkpoint := checkpointFor(t, target)
	initial, err := checkpoint.Initialize(bundle.BaseManifest)
	if err != nil {
		t.Fatal(err)
	}
	writeTransferFile(t, root, "artifact", strings.Replace(base, "last", "destination", 1))
	if _, err := target.Apply(staged, bundle, nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	first, err := checkpoint.Advance(initial.Revision, target.StatePath, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if first.Manifest.RootHash() != bundle.RemoteManifest.RootHash() {
		t.Fatal("checkpoint recorded merged destination instead of source")
	}
	assertTransferFile(t, root, "artifact", strings.Replace(source, "last", "destination", 1))
	// Restart/retry after further destination edits replays the checkpoint update.
	writeTransferFile(t, root, "artifact", strings.Replace(source, "last", "later destination", 1))
	retry, err := checkpoint.Advance(initial.Revision, target.StatePath, bundle)
	if err != nil || !reflect.DeepEqual(first, retry) {
		t.Fatalf("retry = %+v, %v", retry, err)
	}
	// Initialize is also retryable and never resets an advanced checkpoint.
	reopened, err := checkpoint.Initialize(bundle.BaseManifest)
	if err != nil || !reflect.DeepEqual(first, reopened) {
		t.Fatalf("initialize retry = %+v, %v", reopened, err)
	}
	// A second source edit is compared with the accepted source version.
	sender, job := t.TempDir(), t.TempDir()
	writeTransferFile(t, sender, "artifact", source)
	if err := CaptureWorkspaceBaseContext(context.Background(), sender, job, first.Manifest); err != nil {
		t.Fatal(err)
	}
	source2 := strings.Replace(source, "source", "source two", 1)
	writeTransferFile(t, sender, "artifact", source2)
	bundle2, _, err := CollectWorkspaceChangesContext(context.Background(), sender, job, first.Manifest, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	target2 := transferTarget(t, root)
	if _, err := target2.Apply(extractTestBundle(t, job, bundle2), bundle2, nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	second, err := checkpoint.Advance(first.Revision, target2.StatePath, bundle2)
	if err != nil || second.Revision != first.Revision+1 {
		t.Fatalf("second update = %+v, %v", second, err)
	}
	assertTransferFile(t, root, "artifact", strings.Replace(source2, "last", "later destination", 1))
	if _, err := checkpoint.Advance(initial.Revision, target.StatePath, bundle); !errors.Is(err, ErrCheckpointChanged) {
		t.Fatalf("old receipt must not rewind a newer checkpoint: %v", err)
	}
}

func TestCheckpointPartialConflictsAndSelection(t *testing.T) {
	for _, mode := range []string{"refuse", "materialize", "selection"} {
		t.Run(mode, func(t *testing.T) {
			root, bundle, staged := mixedTransferFixture(t)
			target := transferTarget(t, root)
			checkpoint := checkpointFor(t, target)
			initial, err := checkpoint.Initialize(bundle.BaseManifest)
			if err != nil {
				t.Fatal(err)
			}
			var selected map[string]bool
			if mode == "selection" {
				selected = map[string]bool{"clean": true}
			}
			_, applyErr := target.Apply(staged, bundle, selected, ApplyOptions{MaterializeConflicts: mode == "materialize"})
			var conflict *MergeConflictError
			if mode == "selection" && applyErr != nil || mode != "selection" && !errors.As(applyErr, &conflict) {
				t.Fatal(applyErr)
			}
			next, err := checkpoint.Advance(initial.Revision, target.StatePath, bundle)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range bundle.Paths {
				want := exactManifestEntry(bundle.BaseManifest, name)
				if name == "clean" && mode != "refuse" {
					want = exactManifestEntry(bundle.RemoteManifest, name)
				}
				if !reflect.DeepEqual(exactManifestEntry(next.Manifest, name), want) {
					t.Fatalf("wrong checkpoint for %s", name)
				}
			}
			// A refusal consumes an attempt without moving its source baseline.
			// A later attempt at that same baseline must still reject stale revisions.
			if mode == "refuse" {
				target2 := transferTarget(t, root)
				if _, err := target2.Apply(staged, bundle, nil, ApplyOptions{}); !errors.As(err, &conflict) {
					t.Fatal(err)
				}
				if _, err := checkpoint.Advance(next.Revision, target2.StatePath, bundle); err != nil {
					t.Fatal(err)
				}
				if _, err := checkpoint.Advance(initial.Revision, target.StatePath, bundle); !errors.Is(err, ErrCheckpointChanged) {
					t.Fatalf("stale refusal accepted: %v", err)
				}
			}
		})
	}
}

func TestCheckpointScopeAndReceiptValidation(t *testing.T) {
	root, bundle, staged := applyFixture(t, "base\n", "source\n")
	target := transferTarget(t, root)
	checkpoint := checkpointFor(t, target)
	initial, err := checkpoint.Initialize(bundle.BaseManifest)
	if err != nil {
		t.Fatal(err)
	}
	// A separately scoped direction has its own progress.
	otherDirection := checkpointFor(t, target)
	otherDirection.SourceID = "other-direction"
	if _, err := otherDirection.Initialize(bundle.BaseManifest); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"owner", "source", "root"} {
		other := checkpoint
		switch field {
		case "owner":
			other.Owner = "other-owner"
		case "source":
			other.SourceID = "other-source"
		case "root":
			other.Root = t.TempDir()
			other.RootID, _ = applyWorkspaceIdentity(other.Root)
		}
		if _, err := other.Read(); err == nil {
			t.Fatalf("accepted different %s", field)
		}
	}
	// A receipt that has not completed cannot advance the baseline.
	state := transferApplyState{Version: 1, Owner: target.Owner, RootID: target.RootID, BundleRoot: bundle.RootHash(), Paths: bundle.Paths, Pending: NewApplyTransaction()}
	persistTransferState(t, target, state)
	if _, err := checkpoint.Advance(initial.Revision, target.StatePath, bundle); err == nil {
		t.Fatal("accepted incomplete receipt")
	}
	if _, err := target.Apply(staged, bundle, nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	foreign := transferTarget(t, root)
	foreign.Owner = "another-owner"
	if _, err := foreign.Apply(staged, bundle, nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.Advance(initial.Revision, foreign.StatePath, bundle); err == nil {
		t.Fatal("accepted foreign receipt")
	}
	if _, err := checkpoint.Advance(initial.Revision+1, target.StatePath, bundle); !errors.Is(err, ErrCheckpointChanged) {
		t.Fatalf("accepted wrong revision: %v", err)
	}
	if _, err := checkpoint.Advance(initial.Revision, target.StatePath, bundle); err != nil {
		t.Fatal(err)
	}
	unchanged, err := otherDirection.Read()
	if err != nil || !reflect.DeepEqual(initial, unchanged) {
		t.Fatalf("other direction changed: %+v, %v", unchanged, err)
	}
}

func TestCheckpointChecksActualMergeBase(t *testing.T) {
	root, bundle, staged := applyFixture(t, "different base\n", "source\n")
	_, other, _ := applyFixture(t, "recorded base\n", "source\n")
	target := transferTarget(t, root)
	checkpoint := checkpointFor(t, target)
	initial, err := checkpoint.Initialize(other.BaseManifest)
	if err != nil {
		t.Fatal(err)
	}
	// A sender cannot make a different merge base valid by claiming the expected digest.
	bundle.BaselineRoot = initial.Manifest.RootHash()
	if _, err := target.Apply(staged, bundle, nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.Advance(initial.Revision, target.StatePath, bundle); err == nil {
		t.Fatal("accepted mismatched merge base")
	}
	unchanged, err := checkpoint.Read()
	if err != nil || !reflect.DeepEqual(initial, unchanged) {
		t.Fatalf("checkpoint changed after refusal: %+v, %v", unchanged, err)
	}
}

func TestCheckpointRejectsWorkspaceStorage(t *testing.T) {
	root, bundle, _ := applyFixture(t, "base\n", "source\n")
	checkpoint := checkpointFor(t, transferTarget(t, root))
	checkpoint.StatePath = filepath.Join(root, "checkpoint.json")
	if _, err := checkpoint.Initialize(bundle.BaseManifest); err == nil {
		t.Fatal("accepted checkpoint inside destination")
	}
	if _, err := os.Stat(checkpoint.StatePath); !os.IsNotExist(err) {
		t.Fatal("wrote state inside destination")
	}
}

func TestCheckpointMetadataDeletionAndTypeChanges(t *testing.T) {
	baseRoot, sourceRoot := t.TempDir(), t.TempDir()
	for _, root := range []string{baseRoot, sourceRoot} {
		if err := os.Mkdir(filepath.Join(root, "dir"), 0755); err != nil {
			t.Fatal(err)
		}
		writeTransferFile(t, root, "dir/keep", "unchanged")
		writeTransferFile(t, root, "gone", "delete")
		writeTransferFile(t, root, "replace", "file")
	}
	base, err := snapshot.Build(baseRoot, []string{"dir", "dir/keep", "gone", "replace"})
	if err != nil {
		t.Fatal(err)
	}
	job := t.TempDir()
	if err := CaptureWorkspaceBaseContext(context.Background(), sourceRoot, job, base); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(sourceRoot, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(sourceRoot, "gone")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(sourceRoot, "replace")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(sourceRoot, "replace"), 0755); err != nil {
		t.Fatal(err)
	}
	writeTransferFile(t, sourceRoot, "replace/child", "new")
	bundle, _, err := CollectWorkspaceChangesContext(context.Background(), sourceRoot, job, base, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	target := transferTarget(t, baseRoot)
	checkpoint := checkpointFor(t, target)
	initial, err := checkpoint.Initialize(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.Apply(extractTestBundle(t, job, bundle), bundle, nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	next, err := checkpoint.Advance(initial.Revision, target.StatePath, bundle)
	if err != nil {
		t.Fatal(err)
	}
	want, err := snapshot.Build(sourceRoot, []string{"dir", "dir/keep", "replace", "replace/child"})
	if err != nil || !reflect.DeepEqual(want, next.Manifest) {
		t.Fatalf("checkpoint = %+v, want %+v (%v)", next.Manifest, want, err)
	}
}
