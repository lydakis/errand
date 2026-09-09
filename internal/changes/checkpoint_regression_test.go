package changes

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestCheckpointPartialDirectoryDeletion(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	base := proto.Manifest{}
	target := transferTarget(t, destination)
	checkpoint := checkpointFor(t, target)
	version, err := checkpoint.Initialize(base)
	if err != nil {
		t.Fatal(err)
	}
	job := t.TempDir()
	if err := CaptureWorkspaceBaseContext(context.Background(), source, job, base); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{source, destination} {
		if err := os.Mkdir(filepath.Join(root, "dir"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeTransferFile(t, source, "dir/clean", "source file\n")
	writeTransferFile(t, source, "dir/conflict", "incoming\n")
	writeTransferFile(t, destination, "dir/conflict", "destination\n")
	bundle, _, err := CollectWorkspaceChangesContext(context.Background(), source, job, base, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	_, err = target.Apply(extractTestBundle(t, job, bundle), bundle, nil, ApplyOptions{MaterializeConflicts: true})
	var conflict *MergeConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected partial conflict: %v", err)
	}
	assertTransferFile(t, destination, "dir/clean", "source file\n")
	version, err = checkpoint.Advance(version.Revision, target.StatePath, bundle)
	if err != nil {
		t.Fatal(err)
	}
	// User resolves the conflict; sender subsequently deletes the clean file.
	writeTransferFile(t, destination, "dir/conflict", "incoming\n")
	job2 := t.TempDir()
	if err := CaptureWorkspaceBaseContext(context.Background(), source, job2, version.Manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(source, "dir/clean")); err != nil {
		t.Fatal(err)
	}
	bundle2, _, err := CollectWorkspaceChangesContext(context.Background(), source, job2, version.Manifest, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	target2 := transferTarget(t, destination)
	_, err = target2.Apply(extractTestBundle(t, job2, bundle2), bundle2, nil, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "dir/clean")); !os.IsNotExist(err) {
		t.Fatalf("second clean transfer silently lost the source deletion: %v", err)
	}
}

func TestCheckpointReceiptCannotCountTwice(t *testing.T) {
	root, bundle, staged := applyFixture(t, "base\n", "incoming\n")
	target := transferTarget(t, root)
	checkpoint := checkpointFor(t, target)
	initial, err := checkpoint.Initialize(bundle.BaseManifest)
	if err != nil {
		t.Fatal(err)
	}
	writeTransferFile(t, root, "artifact", "destination\n")
	_, err = target.Apply(staged, bundle, nil, ApplyOptions{})
	var conflict *MergeConflictError
	if !errors.As(err, &conflict) {
		t.Fatal(err)
	}
	first, err := checkpoint.Advance(initial.Revision, target.StatePath, bundle)
	if err != nil {
		t.Fatal(err)
	}
	// On restart the caller loads current progress and retries the same receipt.
	current, err := checkpoint.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.Advance(current.Revision, target.StatePath, bundle); !errors.Is(err, ErrCheckpointChanged) {
		t.Fatalf("reused refusal accepted at current revision: %v", err)
	}
	retry, err := checkpoint.Advance(initial.Revision, target.StatePath, bundle)
	if err != nil || !reflect.DeepEqual(first, retry) {
		t.Fatalf("original retry = %+v, %v", retry, err)
	}
	// A distinct application at the same baseline is allowed. Neither old receipt
	// may then be rebound to the current revision, even after an intervening update.
	secondTarget := transferTarget(t, root)
	if _, err := secondTarget.Apply(staged, bundle, nil, ApplyOptions{}); !errors.As(err, &conflict) {
		t.Fatal(err)
	}
	second, err := checkpoint.Advance(first.Revision, secondTarget.StatePath, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.Advance(second.Revision, target.StatePath, bundle); !errors.Is(err, ErrCheckpointChanged) {
		t.Fatalf("old refusal rebound after intervening update: %v", err)
	}
	other := checkpointFor(t, target)
	if _, err := other.Initialize(bundle.BaseManifest); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Advance(0, target.StatePath, bundle); !errors.Is(err, ErrCheckpointChanged) {
		t.Fatalf("receipt rebound to another relationship: %v", err)
	}
}

// A marshaller replaces the storage path after the publisher's preflight and
// before its write. This exercises the real atomic writer without timing races.
type replacingCheckpointRecord struct {
	t           *testing.T
	storage     string
	state       checkpointState
	replacement string // If set, replace a storage alias instead of the directory.
}

func (r replacingCheckpointRecord) MarshalJSON() ([]byte, error) {
	r.t.Helper()
	if err := os.Rename(r.storage, r.storage+"-moved"); err != nil {
		r.t.Fatal(err)
	}
	if r.replacement != "" {
		if err := os.Symlink(r.replacement, r.storage); err != nil {
			r.t.Fatal(err)
		}
	} else {
		if err := os.Mkdir(r.storage, 0700); err != nil {
			r.t.Fatal(err)
		}
	}
	return json.Marshal(r.state)
}

func TestCheckpointRejectsStorageReplacementDuringPublication(t *testing.T) {
	checkpoint := checkpointFor(t, transferTarget(t, t.TempDir()))
	destination, storage, name, err := checkpoint.open(checkpoint.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	defer storage.Close()
	defer os.RemoveAll(storage.path + "-moved")
	state := checkpointState{Version: 1, Owner: checkpoint.Owner, SourceID: checkpoint.SourceID,
		RootID: checkpoint.RootID, InitialRoot: (proto.Manifest{}).RootHash()}
	err = writeVerifiedTransferRecord(destination, storage, name, replacingCheckpointRecord{t: t, storage: storage.path, state: state})
	if err == nil {
		t.Fatal("reported success after storage replacement")
	}
	if _, err := os.Stat(checkpoint.StatePath); !os.IsNotExist(err) {
		t.Fatalf("replacement directory was written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(storage.path+"-moved", name)); err != nil {
		t.Fatalf("did not exercise publication to the original inode: %v", err)
	}
}

func TestCheckpointRejectsRetargetedStorageAlias(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "parent alias", true: "ancestor alias"}[nested], func(t *testing.T) {
			checkpoint := checkpointFor(t, transferTarget(t, t.TempDir()))
			physical, replacement := t.TempDir(), t.TempDir()
			alias := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(physical, alias); err != nil {
				t.Fatal(err)
			}
			parent := alias
			if nested {
				for _, root := range []string{physical, replacement} {
					if err := os.Mkdir(filepath.Join(root, "state"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				parent = filepath.Join(alias, "state")
			}
			checkpoint.StatePath = filepath.Join(parent, "checkpoint.json")
			// Stable aliases must remain usable for initialization, reads and retries.
			initial, err := checkpoint.Initialize(proto.Manifest{})
			if err != nil {
				t.Fatal(err)
			}
			for _, read := range []func() (CheckpointVersion, error){checkpoint.Read, func() (CheckpointVersion, error) { return checkpoint.Initialize(proto.Manifest{}) }} {
				got, err := read()
				if err != nil || !reflect.DeepEqual(initial, got) {
					t.Fatalf("stable alias: %+v, %v", got, err)
				}
			}
			destination, storage, name, err := checkpoint.open(checkpoint.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			defer destination.Close()
			defer storage.Close()
			state, err := checkpoint.read(storage.root, name)
			if err != nil {
				t.Fatal(err)
			}
			err = writeVerifiedTransferRecord(destination, storage, name, replacingCheckpointRecord{t: t, storage: alias, state: state, replacement: replacement})
			if err == nil {
				t.Fatal("reported success after storage alias retargeted")
			}
			if _, err := os.Stat(checkpoint.StatePath); !os.IsNotExist(err) {
				t.Fatalf("wrote to replacement target: %v", err)
			}
			// Read and idempotent return paths use the same guard as publication.
			if err := verifyTransferPaths(destination, storage); err == nil {
				t.Fatal("accepted stale storage alias on read/retry verification")
			}
		})
	}
}

func TestCheckpointDirectoryConflicts(t *testing.T) {
	for _, metadataOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "new directory", true: "existing directory metadata"}[metadataOnly], func(t *testing.T) {
			source, destination, job := t.TempDir(), t.TempDir(), t.TempDir()
			var base proto.Manifest
			for _, root := range []string{source, destination} {
				if err := os.Mkdir(filepath.Join(root, "dir"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if metadataOnly {
				var err error
				base, err = snapshot.Build(source, []string{"dir"})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := CaptureWorkspaceBaseContext(context.Background(), source, job, base); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(source, "dir"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(destination, "dir"), 0711); err != nil {
				t.Fatal(err)
			}
			writeTransferFile(t, source, "dir/clean", "incoming\n")
			writeTransferFile(t, destination, "dir/local", "destination only\n")
			bundle, _, err := CollectWorkspaceChangesContext(context.Background(), source, job, base, proto.SelectionPolicy{}, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			target := transferTarget(t, destination)
			checkpoint := checkpointFor(t, target)
			initial, err := checkpoint.Initialize(base)
			if err != nil {
				t.Fatal(err)
			}
			_, err = target.Apply(extractTestBundle(t, job, bundle), bundle, nil, ApplyOptions{MaterializeConflicts: true})
			var conflict *MergeConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("expected directory conflict: %v", err)
			}
			next, err := checkpoint.Advance(initial.Revision, target.StatePath, bundle)
			if err != nil {
				t.Fatal(err)
			}
			assertTransferFile(t, destination, "dir/local", "destination only\n")
			if len(exactManifestEntry(next.Manifest, "dir/local").Entries) != 0 {
				t.Fatal("destination-only child entered the source checkpoint")
			}
			if !reflect.DeepEqual(exactManifestEntry(next.Manifest, "dir"), exactManifestEntry(base, "dir")) {
				t.Fatal("conflicting directory metadata advanced")
			}
			if metadataOnly {
				assertTransferFile(t, destination, "dir/clean", "incoming\n")
				if !reflect.DeepEqual(exactManifestEntry(next.Manifest, "dir/clean"), exactManifestEntry(bundle.RemoteManifest, "dir/clean")) {
					t.Fatal("independently installed child was forgotten")
				}
			} else {
				if _, err := os.Stat(filepath.Join(destination, "dir/clean")); !os.IsNotExist(err) {
					t.Fatalf("directory conflict partially installed children: %v", err)
				}
				if next.Manifest.RootHash() != base.RootHash() {
					t.Fatal("preserved subtree advanced")
				}
			}
		})
	}
}

func TestCheckpointResumesAfterReceiptBinding(t *testing.T) {
	root, bundle, staged := applyFixture(t, "base\n", "source\n")
	target := transferTarget(t, root)
	checkpoint := checkpointFor(t, target)
	// Both receipt binding and recovery must support stable storage aliases.
	for _, statePath := range []*string{&target.StatePath, &checkpoint.StatePath} {
		alias := filepath.Join(t.TempDir(), "state")
		if err := os.Symlink(filepath.Dir(*statePath), alias); err != nil {
			t.Fatal(err)
		}
		*statePath = filepath.Join(alias, filepath.Base(*statePath))
	}
	if _, err := checkpoint.Initialize(bundle.BaseManifest); err != nil {
		t.Fatal(err)
	}
	initial, err := os.ReadFile(checkpoint.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.Apply(staged, bundle, nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	completed, err := checkpoint.Advance(0, target.StatePath, bundle)
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct a crash after receipt binding but before checkpoint publication.
	if err := os.WriteFile(checkpoint.StatePath, initial, 0600); err != nil {
		t.Fatal(err)
	}
	writeTransferFile(t, root, "artifact", "later local edit\n")
	if _, err := target.Apply(staged, bundle, nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	recovered, err := checkpoint.Advance(0, target.StatePath, bundle)
	if err != nil || !reflect.DeepEqual(completed, recovered) {
		t.Fatalf("recovery = %+v, %v", recovered, err)
	}
	assertTransferFile(t, root, "artifact", "later local edit\n")
}
