package namedcache

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

func openTestStore(t *testing.T, root string, maxBytes int64) *Store {
	t.Helper()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root, maxBytes, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestPersistenceIsolationAndExclusiveLeases(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s := openTestStore(t, root, 1<<20)
	key := Key{Owner: "user-one", Project: "workspace-one", Name: "compiler"}
	job := proto.NewULID()
	data, err := s.Acquire(ctx, key, job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "object"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(ctx, key, proto.NewULID()); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent lease: %v", err)
	}
	if err := s.Release(ctx, key, proto.NewULID()); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("wrong release: %v", err)
	}
	for _, separate := range []Key{{"user-two", key.Project, key.Name}, {key.Owner, "workspace-two", key.Name}, {key.Owner, key.Project, "other"}} {
		otherJob := proto.NewULID()
		other, err := s.Acquire(ctx, separate, otherJob)
		if err != nil {
			t.Fatal(err)
		}
		if other == data {
			t.Fatal("different identity shared storage")
		}
		if _, err := os.Stat(filepath.Join(other, "object")); !os.IsNotExist(err) {
			t.Fatalf("identity saw another cache: %v", err)
		}
		if err := s.Release(ctx, separate, otherJob); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Release(ctx, key, job); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, root, 1<<20)
	data, err = s.Acquire(ctx, key, proto.NewULID())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(data, "object")); err != nil || string(got) != "cached" {
		t.Fatalf("reopened cache: %q %v", got, err)
	}
}

func TestRestartKeepsUnresolvedLeaseProtected(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s := openTestStore(t, root, 1)
	key, job := Key{"owner", "project", "build"}, proto.NewULID()
	data, err := s.Acquire(ctx, key, job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "file"), []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, root, 1)
	if _, err := s.Acquire(ctx, key, proto.NewULID()); !errors.Is(err, ErrBusy) {
		t.Fatalf("restart lost lease: %v", err)
	}
	plan, err := s.GC(ctx, true)
	if err != nil || plan.Protected != 1 || plan.Removed != 0 {
		t.Fatalf("restart gc: %+v %v", plan, err)
	}
	// Only the job lifecycle may settle a lease after process cleanup.
	if err := s.Release(ctx, key, job); err != nil {
		t.Fatal(err)
	}
	result, err := s.GC(ctx, false)
	if err != nil || result.Removed != 1 || result.FreedBytes != 5 {
		t.Fatalf("settled gc: %+v %v", result, err)
	}
}

func TestGCDryRunTTLAndLRU(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir(), 6)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	var paths []string
	for _, name := range []string{"old", "middle", "new"} {
		key, job := Key{"owner", "project", name}, proto.NewULID()
		data, err := s.Acquire(ctx, key, job)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(data, "value"), []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.Release(ctx, key, job); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, data)
		now = now.Add(time.Minute)
	}
	plan, err := s.GC(ctx, true)
	if err != nil || !plan.DryRun || plan.Removed != 2 || plan.FreedBytes != 8 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	for _, data := range paths {
		if _, err := os.Stat(filepath.Join(data, "value")); err != nil {
			t.Fatalf("dry run mutated data: %v", err)
		}
	}
	actual, err := s.GC(ctx, false)
	if err != nil || actual.Removed != plan.Removed || actual.FreedBytes != plan.FreedBytes {
		t.Fatalf("gc: %+v %v", actual, err)
	}
	if _, err := os.Stat(paths[2]); err != nil {
		t.Fatalf("evicted newest: %v", err)
	}
	now = now.Add(2 * time.Hour)
	actual, err = s.GC(ctx, false)
	if err != nil || actual.Removed != 1 {
		t.Fatalf("ttl gc: %+v %v", actual, err)
	}
}

func TestStoreLockAndCancellation(t *testing.T) {
	root := t.TempDir()
	s := openTestStore(t, root, 1024)
	if other, err := Open(root, 1024, time.Hour); err == nil {
		_ = other.Close()
		t.Fatal("two stores opened one root")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	key := Key{"owner", "project", "build"}
	if _, err := s.Acquire(ctx, key, proto.NewULID()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire: %v", err)
	}
	if _, err := s.GC(ctx, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled gc: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(context.Background(), key, proto.NewULID()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed store: %v", err)
	}
}

func TestConcurrentAcquisitionHasOneWinner(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1024)
	results := make(chan error, 20)
	for i := 0; i < cap(results); i++ {
		go func() {
			_, err := s.Acquire(context.Background(), Key{"owner", "project", "shared"}, proto.NewULID())
			results <- err
		}()
	}
	winners := 0
	for i := 0; i < cap(results); i++ {
		if err := <-results; err == nil {
			winners++
		} else if !errors.Is(err, ErrBusy) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("lease winners: %d", winners)
	}
}

func TestFailedReleaseProtectsLeaseUntilDiscard(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir(), 0)
	key, job := Key{"owner", "project", "build"}, proto.NewULID()
	data, err := s.Acquire(ctx, key, job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(data); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, key, job); err == nil {
		t.Fatal("settled missing data")
	}
	result, err := s.GC(ctx, false)
	if err != nil || result.Protected != 1 {
		t.Fatalf("failed release lost protection: %+v %v", result, err)
	}
	if err := s.Discard(ctx, key, proto.NewULID()); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("wrong discard: %v", err)
	}
	if err := s.Discard(ctx, key, job); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(ctx, key, proto.NewULID()); err != nil {
		t.Fatalf("discard did not permit cold start: %v", err)
	}
}

func TestGCRecoversInterruptedWorkWithoutTouchingLiveLease(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir(), 1024)
	key, job := Key{"owner", "project", "build"}, proto.NewULID()
	data, err := s.Acquire(ctx, key, job)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, key, job); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash between atomic retirement and deleting its old tree.
	tomb := ".gc-" + key.hash() + "-" + proto.NewULID()
	if err := s.root.Rename(key.hash(), tomb); err != nil {
		t.Fatal(err)
	}
	creation := ".create-" + proto.NewULID()
	if err := s.root.Mkdir(creation, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err = s.Acquire(ctx, key, proto.NewULID())
	if err != nil {
		t.Fatal(err)
	}
	for _, dryRun := range []bool{true, false} {
		result, err := s.GC(ctx, dryRun)
		if err != nil || result.ReclaimedTemps != 2 || result.Protected != 1 {
			t.Fatalf("recovery: %+v %v", result, err)
		}
		if _, err := s.root.Stat(tomb); os.IsNotExist(err) == dryRun {
			t.Fatalf("dry-run=%t tomb state: %v", dryRun, err)
		}
	}
	if _, err := os.Stat(data); err != nil {
		t.Fatalf("recovery removed replacement cache: %v", err)
	}
}

func TestReadonlyCacheAndDryRunMetadata(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir(), 0)
	key, job := Key{"owner", "project", "readonly"}, proto.NewULID()
	data, err := s.Acquire(ctx, key, job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "file"), []byte("bytes"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(data, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, key, job); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(filepath.Dir(data), "record.json")
	before, err := os.ReadFile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(ctx, true); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(metadata)
	if err != nil || string(after) != string(before) {
		t.Fatal("dry run changed metadata")
	}
	if info, err := os.Stat(data); err != nil || info.Mode().Perm() != 0o500 {
		t.Fatalf("dry run changed permissions: %v %v", info, err)
	}
	result, err := s.GC(ctx, false)
	if err != nil || result.Removed != 1 || result.FreedBytes != 5 {
		t.Fatalf("readonly gc: %+v %v", result, err)
	}
}

func TestReadonlyGitCacheCleanup(t *testing.T) {
	for _, operation := range []string{"evict", "discard", "recover"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			s := openTestStore(t, root, 0)
			// Keep a failed regression from leaving read-only temporary files.
			t.Cleanup(func() {
				_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
					if err == nil && entry.IsDir() {
						return os.Chmod(path, 0o700)
					}
					return nil
				})
			})
			key, job := Key{"owner", "project", "dependencies"}, proto.NewULID()
			data, err := s.Acquire(ctx, key, job)
			if err != nil {
				t.Fatal(err)
			}
			gitDir := filepath.Join(data, "repo", ".git")
			if err := os.MkdirAll(gitDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("cache"), 0o400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(gitDir, 0o500); err != nil {
				t.Fatal(err)
			}
			linkedDir := t.TempDir()
			t.Cleanup(func() { _ = os.Chmod(linkedDir, 0o700) })
			if err := os.Chmod(linkedDir, 0o500); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(linkedDir, filepath.Join(data, "linked")); err != nil {
				t.Fatal(err)
			}
			if operation == "discard" {
				if err := s.Discard(ctx, key, job); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := s.Release(ctx, key, job); err != nil {
					t.Fatal(err)
				}
				if operation == "recover" {
					tomb := ".gc-" + key.hash() + "-" + proto.NewULID()
					if err := s.root.Rename(key.hash(), tomb); err != nil {
						t.Fatal(err)
					}
					gitDir = filepath.Join(root, tomb, "data", "repo", ".git")
				}
				if _, err := s.GC(ctx, true); err != nil {
					t.Fatal(err)
				}
				if info, err := os.Stat(gitDir); err != nil || info.Mode().Perm() != 0o500 {
					t.Fatalf("dry run changed Git directory: %v %v", info, err)
				}
				if _, err := s.GC(ctx, false); err != nil {
					t.Fatal(err)
				}
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 || entries[0].Name() != ".lock" {
				t.Fatalf("cleanup left cache data: %v %v", entries, err)
			}
			if info, err := os.Stat(linkedDir); err != nil || info.Mode().Perm() != 0o500 {
				t.Fatalf("cleanup changed symlink target: %v %v", info, err)
			}
		})
	}
}

func TestInvalidIdentityAndCorruptMetadataAreRefused(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir(), 1024)
	for _, key := range []Key{{"", "project", "build"}, {"owner", "", "build"}, {"owner", "project", "../build"}, {"owner", "project", "."}, {"owner", string([]byte{0xff}), "build"}} {
		if _, err := s.Acquire(ctx, key, proto.NewULID()); err == nil {
			t.Fatalf("accepted invalid key: %+v", key)
		}
	}
	key, job := Key{"owner", "project", "build"}, proto.NewULID()
	data, err := s.Acquire(ctx, key, job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "file"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(data), "record.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(ctx, key, proto.NewULID()); err == nil {
		t.Fatal("reused corrupt metadata")
	}
	if _, err := s.GC(ctx, false); err == nil {
		t.Fatal("collected cache with unverifiable lease")
	}
	if got, err := os.ReadFile(filepath.Join(data, "file")); err != nil || string(got) != "keep" {
		t.Fatalf("corrupt entry was replaced: %q %v", got, err)
	}
}
