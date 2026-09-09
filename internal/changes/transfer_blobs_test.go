package changes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestTransferBlobsReconstructCheckpointAfterSourceDeletion(t *testing.T) {
	ctx := context.Background()
	source, receiver := t.TempDir(), t.TempDir()
	for _, root := range []string{source, receiver} {
		writeTransferFile(t, root, "file", "original\n")
	}
	base, err := snapshot.Build(source, []string{"file"})
	if err != nil {
		t.Fatal(err)
	}
	store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 1 << 20}
	if err := store.Retain(ctx, source, base); err != nil {
		t.Fatal(err)
	}
	checkpoint := checkpointFor(t, transferTarget(t, receiver))
	if _, err := checkpoint.Initialize(base); err != nil {
		t.Fatal(err)
	}
	// The first application changes the receiver and advances the source checkpoint.
	first := t.TempDir()
	if err := store.MaterializeBase(ctx, first, base, 1<<20); err != nil {
		t.Fatal(err)
	}
	writeTransferFile(t, source, "file", "accepted source\n")
	bundle, _, err := CollectWorkspaceChangesContext(ctx, source, first, base, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	staged := extractTestBundle(t, first, bundle)
	if err := store.Retain(ctx, filepath.Join(staged, "remote"), bundle.RemoteManifest); err != nil {
		t.Fatal(err)
	}
	target := transferTarget(t, receiver)
	if _, err := target.Apply(staged, bundle, nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	next, err := checkpoint.Advance(0, target.StatePath, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(source, "file")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTree(first); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTree(staged); err != nil {
		t.Fatal(err)
	}
	// Only the checkpoint's accepted content remains pinned; old bodies can go.
	freed, err := store.Prune(ctx, []proto.Manifest{next.Manifest}, false)
	if err != nil || freed.RemovedBlobs != 1 {
		t.Fatalf("prune = %+v, %v", freed, err)
	}
	restarted := TransferBlobStore{Directory: store.Directory, MaxBytes: store.MaxBytes}
	second := t.TempDir()
	if err := restarted.MaterializeBase(ctx, second, next.Manifest, 1<<20); err != nil {
		t.Fatal(err)
	}
	assertTransferFile(t, workspaceBasePath(second), "file", "accepted source\n")
	deletion, _, err := CollectWorkspaceChangesContext(ctx, source, second, next.Manifest, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transferTarget(t, receiver).Apply(extractTestBundle(t, second, deletion), deletion, nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(receiver, "file")); !os.IsNotExist(err) {
		t.Fatalf("deletion did not reach receiver: %v", err)
	}
}

func TestTransferBlobsDeduplicateAndPreserveTreeMetadata(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dir/one", "dir/two"} {
		writeTransferFile(t, source, name, "shared\n")
	}
	if err := os.Symlink("one", filepath.Join(source, "dir/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "dir/one"), 0755); err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshot.Build(source, []string{"dir", "dir/link", "dir/one", "dir/two"})
	if err != nil {
		t.Fatal(err)
	}
	store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 7}
	if err := store.Retain(ctx, source, manifest); err != nil {
		t.Fatal(err)
	}
	if err := store.Retain(ctx, source, manifest); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.Blobs != 1 || stats.Bytes != 7 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
	job := t.TempDir()
	if err := store.MaterializeBase(ctx, job, manifest, 7); !errors.Is(err, ErrByteLimitExceeded) {
		t.Fatalf("deduplication bypassed logical limit: %v", err)
	}
	if entries, err := os.ReadDir(job); err != nil || len(entries) != 0 {
		t.Fatalf("published oversized reconstruction: %v, %v", entries, err)
	}
	if err := store.MaterializeBase(ctx, job, manifest, 14); err != nil {
		t.Fatal(err)
	}
	got, err := snapshot.Build(workspaceBasePath(job), []string{"dir", "dir/link", "dir/one", "dir/two"})
	if err != nil || !reflect.DeepEqual(got, manifest) {
		t.Fatalf("tree = %+v, %v", got, err)
	}
	writeTransferFile(t, workspaceBasePath(job), "dir/one", "local edit\n")
	// Reconstructed files must not be writable hardlinks into shared retained data.
	next := t.TempDir()
	if err := store.MaterializeBase(ctx, next, manifest, 1<<20); err != nil {
		t.Fatal(err)
	}
	assertTransferFile(t, workspaceBasePath(next), "dir/one", "shared\n")
	if err := store.MaterializeBase(ctx, job, manifest, 1<<20); err == nil {
		t.Fatal("overwrote existing change base")
	}
	assertTransferFile(t, workspaceBasePath(job), "dir/one", "local edit\n")
}

func TestTransferBlobsFailClosed(t *testing.T) {
	for _, failure := range []string{"quota", "source changed", "corrupt blob", "missing blob", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			source := t.TempDir()
			writeTransferFile(t, source, "file", "source\n")
			writeTransferFile(t, source, "later", "another body\n")
			manifest, err := snapshot.Build(source, []string{"file", "later"})
			if err != nil {
				t.Fatal(err)
			}
			store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 1 << 20}
			job := t.TempDir()
			switch failure {
			case "quota":
				store.MaxBytes = 1
			case "source changed":
				writeTransferFile(t, source, "file", "changed\n")
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err = store.Retain(ctx, source, manifest)
			if failure == "corrupt blob" || failure == "missing blob" {
				if err != nil {
					t.Fatal(err)
				}
				blob := filepath.Join(store.Directory, manifest.Entries[1].SHA256)
				if failure == "corrupt blob" {
					err = os.WriteFile(blob, []byte("corrupt body\n"), 0600)
				} else {
					err = os.Remove(blob)
				}
				if err != nil {
					t.Fatal(err)
				}
				err = store.MaterializeBase(ctx, job, manifest, 1<<20)
			}
			if err == nil {
				t.Fatal("accepted invalid transfer content")
			}
			if failure == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if failure == "quota" && !errors.Is(err, ErrByteLimitExceeded) {
				t.Fatal(err)
			}
			if failure == "corrupt blob" || failure == "missing blob" {
				if entries, err := os.ReadDir(job); err != nil || len(entries) != 0 {
					t.Fatalf("left a partial reconstruction: %v, %v", entries, err)
				}
			} else {
				assertTransferBlobsComplete(t, store.Directory)
			}
		})
	}
}

func TestTransferBlobsRetainMixedCheckpointAfterConflicts(t *testing.T) {
	ctx := context.Background()
	root, bundle, staged := mixedTransferFixture(t)
	store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 1 << 20}
	if err := store.Retain(ctx, filepath.Join(staged, "base"), bundle.BaseManifest); err != nil {
		t.Fatal(err)
	}
	if err := store.Retain(ctx, filepath.Join(staged, "remote"), bundle.RemoteManifest); err != nil {
		t.Fatal(err)
	}
	target := transferTarget(t, root)
	checkpoint := checkpointFor(t, target)
	if _, err := checkpoint.Initialize(bundle.BaseManifest); err != nil {
		t.Fatal(err)
	}
	_, err := target.Apply(staged, bundle, nil, ApplyOptions{MaterializeConflicts: true})
	var conflict *MergeConflictError
	if !errors.As(err, &conflict) {
		t.Fatal(err)
	}
	next, err := checkpoint.Advance(0, target.StatePath, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveTree(staged); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTree(root); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prune(ctx, []proto.Manifest{next.Manifest}, false); err != nil {
		t.Fatal(err)
	}
	job := t.TempDir()
	if err := store.MaterializeBase(ctx, job, next.Manifest, 1<<20); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range next.Manifest.Entries {
		paths = append(paths, entry.Path)
	}
	actual, err := snapshot.Build(workspaceBasePath(job), paths)
	if err != nil || !reflect.DeepEqual(actual, next.Manifest) {
		t.Fatalf("mixed baseline = %+v, %v", actual, err)
	}
}

func TestTransferBlobsPruneHonorsAllPins(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 1 << 20}
	var manifests []proto.Manifest
	for _, content := range []string{"checkpoint\n", "inflight\n", "unreferenced\n"} {
		writeTransferFile(t, source, "file", content)
		manifest, err := snapshot.Build(source, []string{"file"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Retain(ctx, source, manifest); err != nil {
			t.Fatal(err)
		}
		manifests = append(manifests, manifest)
	}
	before, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dry, err := store.Prune(ctx, manifests[:2], true)
	if err != nil || dry.RemovedBlobs != 1 || dry.FreedBytes != int64(len("unreferenced\n")) {
		t.Fatalf("dry run = %+v, %v", dry, err)
	}
	after, err := store.Stats(ctx)
	if err != nil || before != after {
		t.Fatalf("dry run changed storage: %+v, %v", after, err)
	}
	actual, err := store.Prune(ctx, manifests[:2], false)
	if err != nil || actual != dry {
		t.Fatalf("prune = %+v, %v", actual, err)
	}
	for _, manifest := range manifests[:2] {
		if err := store.MaterializeBase(ctx, t.TempDir(), manifest, 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	// Invalid or missing references must be rejected before any removal.
	before, err = store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prune(ctx, manifests[2:], false); err == nil {
		t.Fatal("accepted missing pinned content")
	}
	after, err = store.Stats(ctx)
	if err != nil || after != before {
		t.Fatal("failed pin validation removed data")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(store.Directory, transferBlobTempPrefix+"trap")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prune(ctx, nil, false); err == nil {
		t.Fatal("followed unexpected storage symlink")
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "keep" {
		t.Fatal("changed symlink target")
	}
}

func TestTransferBlobsBoundReconstructionAndKeepRestrictedModes(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	writeTransferFile(t, source, "dir/file", "private\n")
	manifest, err := snapshot.Build(source, []string{"dir", "dir/file"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range manifest.Entries {
		manifest.Entries[i].Mode = 0
	}
	if err := os.Chmod(filepath.Join(source, "dir/file"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "dir"), 0); err != nil {
		t.Fatal(err)
	}
	defer RemoveTree(source)
	store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 1 << 20}
	if err := store.Retain(ctx, source, manifest); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(source, "dir"))
	if err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("source permissions changed: %v", err)
	}
	job := t.TempDir()
	if err := store.MaterializeBase(ctx, job, manifest, 1); !errors.Is(err, ErrByteLimitExceeded) {
		t.Fatalf("unbounded reconstruction: %v", err)
	}
	if _, err := os.Stat(workspaceBasePath(job)); !os.IsNotExist(err) {
		t.Fatal("published oversized baseline")
	}
	if err := store.MaterializeBase(ctx, job, manifest, 1<<20); err != nil {
		t.Fatal(err)
	}
	defer RemoveTree(job)
	info, err = os.Stat(filepath.Join(workspaceBasePath(job), "dir"))
	if err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("directory mode changed: %v", err)
	}
	access, err := makeManifestAccessible(workspaceBasePath(job), manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer access.restore()
	got, err := snapshot.Build(workspaceBasePath(job), []string{"dir", "dir/file"})
	if err != nil {
		t.Fatal(err)
	}
	access.logicalize(&got)
	if !reflect.DeepEqual(manifest, got) {
		t.Fatal("reconstructed metadata or contents changed")
	}
}
