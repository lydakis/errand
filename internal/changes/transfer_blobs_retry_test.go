package changes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func assertTransferBlobsComplete(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(body)
		if entry.Name() != hex.EncodeToString(hash[:]) {
			t.Fatalf("unpublished or invalid body %q", entry.Name())
		}
	}
}

type cancelTransferBlobReader struct {
	io.Reader
	cancel context.CancelFunc
}

func (r cancelTransferBlobReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.cancel()
	}
	return n, err
}

func TestTransferBlobsCancelInsertionMidCopy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := strings.Repeat("content", 10000)
	hash := sha256.Sum256([]byte(body))
	entry := proto.ManifestEntry{Path: "file", Type: proto.EntryFile, Mode: 0600, Size: int64(len(body)), SHA256: hex.EncodeToString(hash[:])}
	store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: entry.Size}
	storage, err := store.open()
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	reader := cancelTransferBlobReader{Reader: strings.NewReader(body), cancel: cancel}
	if _, err := prepareTransferBlob(ctx, storage, reader, entry, syncStagedData); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-copy cancellation = %v", err)
	}
	entries, err := os.ReadDir(store.Directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial insertion survived: %v, %v", entries, err)
	}
}

func TestTransferBlobsRetryUsesStoredBodies(t *testing.T) {
	for _, scenario := range []string{"lowered quota", "removed source", "partly retained"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			source := t.TempDir()
			writeTransferFile(t, source, "old", "stored")
			manifest, err := snapshot.Build(source, []string{"old"})
			if err != nil {
				t.Fatal(err)
			}
			store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 100}
			if err := store.Retain(ctx, source, manifest); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "lowered quota":
				store.MaxBytes = 1
			case "removed source":
				if err := os.RemoveAll(source); err != nil {
					t.Fatal(err)
				}
			case "partly retained":
				writeTransferFile(t, source, "new", "additional")
				manifest, err = snapshot.Build(source, []string{"new", "old"})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(source, "old")); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Retain(ctx, source, manifest); err != nil {
				t.Fatal(err)
			}
			job := t.TempDir()
			if err := store.MaterializeBase(ctx, job, manifest, 100); err != nil {
				t.Fatal(err)
			}
			assertTransferFile(t, workspaceBasePath(job), "old", "stored")
			if scenario == "partly retained" {
				assertTransferFile(t, workspaceBasePath(job), "new", "additional")
			}
			if scenario == "lowered quota" {
				writeTransferFile(t, source, "old", "more bytes")
				next, err := snapshot.Build(source, []string{"old"})
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Retain(ctx, source, next); !errors.Is(err, ErrByteLimitExceeded) {
					t.Fatalf("growth above quota = %v", err)
				}
			}
		})
	}
}

func TestTransferBlobsCachedCorruptionIsNotRepaired(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	writeTransferFile(t, source, "file", "good")
	manifest, err := snapshot.Build(source, []string{"file"})
	if err != nil {
		t.Fatal(err)
	}
	store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 100}
	if err := store.Retain(ctx, source, manifest); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(store.Directory, manifest.Entries[0].SHA256)
	if err := os.WriteFile(blob, []byte("evil"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Retain(ctx, source, manifest); err == nil {
		t.Fatal("silently repaired corrupt stored body")
	}
	assertTransferFile(t, store.Directory, manifest.Entries[0].SHA256, "evil")
}

func TestTransferBlobsRetainReclaimsAbandonedBatch(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	for _, name := range []string{"a", "b", "cached"} {
		writeTransferFile(t, source, name, strings.Repeat(name[:1], 8))
	}
	manifest, err := snapshot.Build(source, []string{"a", "b", "cached"})
	if err != nil {
		t.Fatal(err)
	}
	store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 24}
	for _, e := range manifest.Entries {
		name := transferBlobTempPrefix + e.Path
		if e.Path == "cached" {
			name = e.SHA256
		}
		writeTransferFile(t, store.Directory, name, strings.Repeat(e.Path[:1], 8))
	}
	if err := store.Retain(ctx, source, manifest); err != nil {
		t.Fatalf("retry after abandoned batch: %v", err)
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.Blobs != 3 || stats.Bytes != 24 {
		t.Fatalf("stats after recovery = %+v, %v", stats, err)
	}
	for _, e := range manifest.Entries {
		assertTransferFile(t, store.Directory, e.SHA256, strings.Repeat(e.Path[:1], 8))
	}
}

func TestTransferBlobsPruneAbandonedInsertionThenRetry(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	writeTransferFile(t, source, "file", "body")
	manifest, err := snapshot.Build(source, []string{"file"})
	if err != nil {
		t.Fatal(err)
	}
	store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 4}
	writeTransferFile(t, store.Directory, transferBlobTempPrefix+"abandoned", "body")
	before, err := store.Stats(ctx)
	if err != nil || before.Blobs != 0 || before.Bytes != 4 {
		t.Fatalf("stats = %+v, %v", before, err)
	}
	dry, err := store.Prune(ctx, nil, true)
	if err != nil || dry.RemovedBlobs != 0 || dry.FreedBytes != 4 {
		t.Fatalf("dry prune = %+v, %v", dry, err)
	}
	after, err := store.Stats(ctx)
	if err != nil || before != after {
		t.Fatalf("dry run changed storage: %+v, %v", after, err)
	}
	actual, err := store.Prune(ctx, nil, false)
	if err != nil || actual != dry {
		t.Fatalf("prune = %+v, %v", actual, err)
	}
	if err := store.Retain(ctx, source, manifest); err != nil {
		t.Fatal(err)
	}
}

func TestTransferBlobsAbandonedCleanupGuards(t *testing.T) {
	for _, scenario := range []string{"canceled", "overlapping source", "nonregular temporary"} {
		t.Run(scenario, func(t *testing.T) {
			source := t.TempDir()
			writeTransferFile(t, source, "file", "body")
			manifest, err := snapshot.Build(source, []string{"file"})
			if err != nil {
				t.Fatal(err)
			}
			store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 100}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "canceled":
				cancel()
			case "overlapping source":
				store.Directory = filepath.Join(source, "store")
				if err := os.Mkdir(store.Directory, 0700); err != nil {
					t.Fatal(err)
				}
			case "nonregular temporary":
				if err := os.Symlink(filepath.Join(source, "file"), filepath.Join(store.Directory, transferBlobTempPrefix+"link")); err != nil {
					t.Fatal(err)
				}
			}
			writeTransferFile(t, store.Directory, transferBlobTempPrefix+"abandoned", "body")
			if err := store.Retain(ctx, source, manifest); err == nil {
				t.Fatal("accepted invalid retention")
			}
			assertTransferFile(t, store.Directory, transferBlobTempPrefix+"abandoned", "body")
			assertTransferFile(t, source, "file", "body")
		})
	}
}

func TestTransferBlobsRejectOverlappingDirectories(t *testing.T) {
	for _, scenario := range []string{"retain store inside source", "base store inside job", "base job inside store"} {
		t.Run(scenario, func(t *testing.T) {
			outer := t.TempDir()
			inner := filepath.Join(outer, "inner")
			if err := os.Mkdir(inner, 0700); err != nil {
				t.Fatal(err)
			}
			store := TransferBlobStore{Directory: inner, MaxBytes: 100}
			ctx := context.Background()
			var err error
			if scenario == "retain store inside source" {
				writeTransferFile(t, outer, "file", "body")
				manifest, buildErr := snapshot.Build(outer, []string{"file"})
				if buildErr != nil {
					t.Fatal(buildErr)
				}
				err = store.Retain(ctx, outer, manifest)
			} else {
				job := outer
				if scenario == "base job inside store" {
					store.Directory, job = outer, inner
				}
				err = store.MaterializeBase(ctx, job, proto.Manifest{}, 100)
			}
			if err == nil {
				t.Fatal("accepted overlapping directories")
			}
			entries, err := os.ReadDir(inner)
			if err != nil || len(entries) != 0 {
				t.Fatalf("changed inner directory: %v, %v", entries, err)
			}
			if _, err := os.Stat(filepath.Join(outer, "change-base")); !os.IsNotExist(err) {
				t.Fatalf("published baseline: %v", err)
			}
		})
	}
}
