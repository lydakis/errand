package changes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func transferBlobManifest(t *testing.T, source string, contents ...string) proto.Manifest {
	t.Helper()
	var paths []string
	for i, content := range contents {
		paths = append(paths, string(rune('a'+i)))
		writeTransferFile(t, source, paths[i], content)
	}
	m, err := snapshot.Build(source, paths)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func assertTransferBlobUsage(t *testing.T, directory string, want int64) {
	t.Helper()
	got, ok := transferBlobUsageFile(t, filepath.Join(directory, transferBlobUsageName))
	if !ok || got != want {
		t.Fatalf("usage record = %d (present %v), want %d", got, ok, want)
	}
}

// expectNoTransferBlobUsage does not stop the test, so staging workers may call it.
func expectNoTransferBlobUsage(t *testing.T, directory, when string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(directory, transferBlobUsageName)); !os.IsNotExist(err) {
		t.Errorf("usage record present %s: %v", when, err)
	}
}

func TestTransferBlobUsageBudgetsWithoutListing(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 12}
	if err := store.Retain(ctx, source, transferBlobManifest(t, source, "first!")); err != nil {
		t.Fatal(err)
	}
	assertTransferBlobUsage(t, store.Directory, 6)
	// Retain looks up only the bodies it needs. An entry that a full scan
	// rejects goes unnoticed until Stats or Prune lists the store.
	writeTransferFile(t, store.Directory, "unexpected", "not a body")
	if err := store.Retain(ctx, source, transferBlobManifest(t, source, "first!", "second")); err != nil {
		t.Fatal(err)
	}
	assertTransferBlobUsage(t, store.Directory, 12)
	if _, err := store.Stats(ctx); err == nil {
		t.Fatal("stats accepted an unexpected entry")
	}
	if err := os.Remove(filepath.Join(store.Directory, "unexpected")); err != nil {
		t.Fatal(err)
	}
	// Capacity is exactly used; a refusal changes nothing, so the record stays.
	if err := store.Retain(ctx, source, transferBlobManifest(t, source, "third!")); !errors.Is(err, ErrByteLimitExceeded) {
		t.Fatalf("growth above capacity = %v", err)
	}
	assertTransferBlobUsage(t, store.Directory, 12)
	if err := store.Retain(ctx, source, transferBlobManifest(t, source, "")); err != nil {
		t.Fatalf("empty body at capacity: %v", err)
	}
	assertTransferBlobUsage(t, store.Directory, 12)
	assertTransferBlobsComplete(t, store.Directory)
}

func TestTransferBlobUsageRemovedBeforeInsertion(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 100}
	if err := store.Retain(ctx, source, transferBlobManifest(t, source, "stored")); err != nil {
		t.Fatal(err)
	}
	assertTransferBlobUsage(t, store.Directory, 6)
	members, barriers := 0, 0
	err := store.retain(ctx, source, transferBlobManifest(t, source, "stored", "additional"), func(f *os.File) error {
		members++
		// A crash from here on must leave no record, so the next Retain
		// scans and reclaims this insertion file.
		expectNoTransferBlobUsage(t, store.Directory, "during insertion")
		return syncStagedData(f)
	}, func(root *os.Root) error {
		barriers++
		// Naming the record before the final barrier could count bodies whose
		// renames a crash then loses.
		expectNoTransferBlobUsage(t, store.Directory, "at a barrier")
		return syncApplyRootDirectory(root, ".")
	})
	if err != nil {
		t.Fatal(err)
	}
	// Only the missing body and the new record are written; the stored body
	// is verified in place.
	if members != 2 || barriers != 2 {
		t.Fatalf("member syncs = %d, barriers = %d; want 2, 2", members, barriers)
	}
	assertTransferBlobUsage(t, store.Directory, 16)
	assertTransferBlobsComplete(t, store.Directory)
}

func TestTransferBlobUsageAfterFailedRetention(t *testing.T) {
	for _, failure := range []string{"source changed", "data barrier", "publication", "stored corruption"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			source := t.TempDir()
			store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 16}
			first := transferBlobManifest(t, source, "stored")
			if err := store.Retain(ctx, source, first); err != nil {
				t.Fatal(err)
			}
			next := transferBlobManifest(t, source, "stored", "additional")
			injected := errors.New("sync failed")
			barriers := 0
			switch failure {
			case "source changed":
				writeTransferFile(t, source, "b", "changed!!!")
			case "stored corruption":
				writeTransferFile(t, store.Directory, first.Entries[0].SHA256, "STORED")
			}
			err := store.retain(ctx, source, next, syncStagedData, func(root *os.Root) error {
				barriers++
				if failure == "data barrier" && barriers == 1 || failure == "publication" && barriers == 2 {
					return injected
				}
				return syncApplyRootDirectory(root, ".")
			})
			if err == nil {
				t.Fatal("accepted failed retention")
			}
			// Nothing records the store until a scan rebuilds it.
			expectNoTransferBlobUsage(t, store.Directory, "after failure")
			if failure == "stored corruption" {
				writeTransferFile(t, store.Directory, first.Entries[0].SHA256, "stored")
			}
			writeTransferFile(t, source, "b", "additional")
			if err := store.Retain(ctx, source, next); err != nil {
				t.Fatal(err)
			}
			assertTransferBlobUsage(t, store.Directory, 16)
			assertTransferBlobsComplete(t, store.Directory)
		})
	}
}

func TestTransferBlobUsageScanReclaimsAbandonedInsertions(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	m := transferBlobManifest(t, source, "stored", "missing!")
	store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 14}
	// A crash mid-retention leaves bodies, insertion files (including an
	// unnamed record) and no record.
	writeTransferFile(t, store.Directory, m.Entries[0].SHA256, "stored")
	writeTransferFile(t, store.Directory, transferBlobTempPrefix+"body", "missing!")
	writeTransferFile(t, store.Directory, transferBlobTempPrefix+"usage", `{"bytes":0}`)
	if err := store.Retain(ctx, source, m); err != nil {
		t.Fatalf("budgeted abandoned insertions: %v", err)
	}
	assertTransferBlobUsage(t, store.Directory, 14)
	assertTransferBlobsComplete(t, store.Directory)

	// A scan that finds every body stored still records the total.
	if err := os.Remove(filepath.Join(store.Directory, transferBlobUsageName)); err != nil {
		t.Fatal(err)
	}
	members := 0
	if err := store.retain(ctx, "/unavailable", m, func(f *os.File) error { members++; return syncStagedData(f) }, func(root *os.Root) error { return syncApplyRootDirectory(root, ".") }); err != nil {
		t.Fatal(err)
	}
	if members != 1 {
		t.Fatalf("member syncs = %d, want only the record", members)
	}
	assertTransferBlobUsage(t, store.Directory, 14)
	// With the record in place, a retry writes nothing.
	members = 0
	if err := store.retain(ctx, "/unavailable", m, func(f *os.File) error { members++; return syncStagedData(f) }, func(root *os.Root) error { return syncApplyRootDirectory(root, ".") }); err != nil || members != 0 {
		t.Fatalf("cached retry: %d member syncs, %v", members, err)
	}
}

// Only a system crash that keeps insertion files but loses the record's
// earlier removal can leave both. The record still counts published bodies
// exactly; Prune reclaims the insertions.
func TestTransferBlobUsageNeverBudgetsInsertions(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 12}
	first := transferBlobManifest(t, source, "stored")
	if err := store.Retain(ctx, source, first); err != nil {
		t.Fatal(err)
	}
	writeTransferFile(t, store.Directory, transferBlobTempPrefix+"survivor", "survivor")
	if err := store.Retain(ctx, source, transferBlobManifest(t, source, "stored", "second")); err != nil {
		t.Fatalf("budgeted an insertion file: %v", err)
	}
	assertTransferBlobUsage(t, store.Directory, 12)
	result, err := store.Prune(ctx, []proto.Manifest{first}, false)
	if err != nil || result.RemovedBlobs != 1 || result.FreedBytes != int64(len("second")+len("survivor")) {
		t.Fatalf("prune = %+v, %v", result, err)
	}
	assertTransferBlobsComplete(t, store.Directory)
}

func TestTransferBlobUsageRebuiltAfterPrune(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 12}
	first := transferBlobManifest(t, source, "first!")
	if err := store.Retain(ctx, source, first); err != nil {
		t.Fatal(err)
	}
	second := transferBlobManifest(t, source, "second")
	if err := store.Retain(ctx, source, second); err != nil {
		t.Fatal(err)
	}
	third := transferBlobManifest(t, source, "third!")
	if err := store.Retain(ctx, source, third); !errors.Is(err, ErrByteLimitExceeded) {
		t.Fatalf("growth above capacity = %v", err)
	}
	// Collection runs in another process (gc changes) with its own store value;
	// only the directory is shared.
	collector := TransferBlobStore{Directory: store.Directory, MaxBytes: store.MaxBytes}
	if _, err := collector.Prune(ctx, []proto.Manifest{second}, true); err != nil {
		t.Fatal(err)
	}
	assertTransferBlobUsage(t, store.Directory, 12)
	if _, err := collector.Prune(ctx, []proto.Manifest{first, third}, false); err == nil {
		t.Fatal("accepted a missing pin")
	}
	assertTransferBlobUsage(t, store.Directory, 12)
	barriers := 0
	unpinned := filepath.Join(store.Directory, first.Entries[0].SHA256)
	result, err := collector.prune(ctx, []proto.Manifest{second}, false, func(root *os.Root) error {
		barriers++
		expectNoTransferBlobUsage(t, store.Directory, "while pruning")
		if _, err := os.Lstat(unpinned); barriers == 1 && err != nil {
			t.Errorf("removed a body before the record's removal was durable: %v", err)
		}
		return syncApplyRootDirectory(root, ".")
	})
	if err != nil || result.RemovedBlobs != 1 || barriers != 2 {
		t.Fatalf("prune = %+v, %v after %d barriers", result, err, barriers)
	}
	// A record kept across the prune would still claim 12 bytes.
	if err := store.Retain(ctx, source, third); err != nil {
		t.Fatalf("retain after prune: %v", err)
	}
	assertTransferBlobUsage(t, store.Directory, 12)
	assertTransferBlobsComplete(t, store.Directory)
}

func TestTransferBlobUsageInvalidRecord(t *testing.T) {
	for _, record := range []string{"garbage", `{"bytes":-1}`, "null", "directory", "symlink"} {
		t.Run(record, func(t *testing.T) {
			ctx := context.Background()
			source := t.TempDir()
			store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 12}
			first := transferBlobManifest(t, source, "first!")
			if err := store.Retain(ctx, source, first); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(store.Directory, transferBlobUsageName)
			if err := os.Remove(name); err != nil {
				t.Fatal(err)
			}
			var err error
			switch record {
			case "directory":
				err = os.Mkdir(name, 0700)
			case "symlink":
				err = os.Symlink(first.Entries[0].SHA256, name)
			default:
				err = os.WriteFile(name, []byte(record), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = store.Retain(ctx, source, transferBlobManifest(t, source, "second"))
			if record == "directory" || record == "symlink" {
				// Storage is private; a record that is not a regular file fails closed.
				if err == nil {
					t.Fatal("accepted a non-regular usage record")
				}
				return
			}
			// A malformed record is rebuilt from a scan rather than trusted.
			if err != nil {
				t.Fatal(err)
			}
			assertTransferBlobUsage(t, store.Directory, 12)
			assertTransferBlobsComplete(t, store.Directory)
		})
	}
}

func TestTransferBlobUsageLookupRejectsNonRegularBody(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 100}
	if err := store.Retain(ctx, source, transferBlobManifest(t, source, "stored")); err != nil {
		t.Fatal(err)
	}
	m := transferBlobManifest(t, source, "stored", "directory")
	if err := os.Mkdir(filepath.Join(store.Directory, m.Entries[1].SHA256), 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.Retain(ctx, source, m); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("accepted a non-regular body: %v", err)
	}
	assertTransferBlobUsage(t, store.Directory, 6)
}
