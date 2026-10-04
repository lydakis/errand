package changes

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lydakis/errand/internal/snapshot"
)

func TestTransferSourceSyncRestoresModesBeforePublication(t *testing.T) {
	for _, failure := range []string{"none", "member", "publication"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "!dir"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "!dir", "file"), []byte("body"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("file", filepath.Join(root, "!dir", "link")); err != nil {
				t.Fatal(err)
			}
			manifest, err := snapshot.Build(root, []string{"!dir", "!dir/file", "!dir/link"})
			if err != nil {
				t.Fatal(err)
			}
			// Neither the file nor its parent may remain readable after staging.
			if err := os.Chmod(filepath.Join(root, "!dir", "file"), 0); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(root, "!dir"), 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				os.Chmod(root, 0700)
				os.Chmod(filepath.Join(root, "!dir"), 0700)
				os.Chmod(filepath.Join(root, "!dir", "file"), 0600)
			})
			if err := os.Chmod(root, 0); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("sync failed")
			members, barriers := 0, 0
			err = syncTransferSource(root, manifest, func(f *os.File) error {
				members++
				info, err := f.Stat()
				if err != nil {
					return err
				}
				if info.Mode().Perm() != 0 {
					t.Errorf("member flushed before restoring mode: %v", info.Mode())
				}
				if failure == "member" {
					return injected
				}
				return syncStagedData(f)
			}, func(f *os.File) error {
				barriers++
				if members != 3 {
					t.Errorf("publication after %d members, want 3", members)
				}
				if failure == "publication" {
					return injected
				}
				return f.Sync()
			})
			if failure == "none" && err != nil || failure != "none" && !errors.Is(err, injected) {
				t.Fatalf("sync result: %v", err)
			}
			if failure == "member" && barriers != 0 {
				t.Fatal("published after member failure")
			}
			if failure != "member" && barriers != 1 {
				t.Fatalf("full flushes = %d", barriers)
			}
			info, err := os.Stat(root)
			if err != nil || info.Mode().Perm() != 0 {
				t.Fatalf("directory mode not restored: %v, %v", info, err)
			}
		})
	}
}

func TestTransferBlobBatchPublicationAndRetry(t *testing.T) {
	for _, failure := range []string{"none", "member", "data barrier", "publication", "canceled before publication"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			writeTransferFile(t, root, "a", "first")
			writeTransferFile(t, root, "b", "second")
			writeTransferFile(t, root, "duplicate", "first")
			m, err := snapshot.Build(root, []string{"a", "b", "duplicate"})
			if err != nil {
				t.Fatal(err)
			}
			store := TransferBlobStore{Directory: t.TempDir(), MaxBytes: 100}
			injected := errors.New("sync failed")
			var members atomic.Int32
			barriers := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err = store.retain(ctx, root, m, func(f *os.File) error {
				members.Add(1)
				if failure == "member" {
					return injected
				}
				return syncStagedData(f)
			}, func(root *os.Root) error {
				barriers++
				// Two distinct bodies plus the store's new usage record.
				if members.Load() != 3 {
					t.Errorf("publication after %d member syncs, want 3", members.Load())
				}
				entries, err := os.ReadDir(store.Directory)
				if err != nil {
					return err
				}
				if len(entries) != 3 {
					t.Errorf("entries at barrier %d = %d, want 3", barriers, len(entries))
				}
				var usage int
				for _, entry := range entries {
					name := entry.Name()
					if strings.HasPrefix(name, transferBlobTempPrefix) {
						// The record stays unnamed until after the final barrier.
						if bytes, ok := transferBlobUsageFile(t, filepath.Join(store.Directory, name)); ok && bytes == int64(len("first")+len("second")) {
							usage++
						} else if barriers != 1 {
							t.Errorf("unpublished body %q after data barrier", name)
						}
					} else if barriers == 1 {
						t.Errorf("published %q before data barrier", name)
					} else if body, err := os.ReadFile(filepath.Join(store.Directory, name)); err != nil || fmt.Sprintf("%x", sha256.Sum256(body)) != name {
						t.Errorf("invalid published body %q: %v", name, err)
					}
				}
				if usage != 1 {
					t.Errorf("pending usage records at barrier %d = %d, want 1", barriers, usage)
				}
				if barriers == 1 {
					if failure == "data barrier" {
						return injected
					}
					if failure == "canceled before publication" {
						cancel()
					}
				} else if failure == "publication" {
					return injected
				}
				return syncApplyRootDirectory(root, ".")
			})
			expected := injected
			if failure == "canceled before publication" {
				expected = context.Canceled
			}
			if failure == "none" && err != nil || failure != "none" && !errors.Is(err, expected) {
				t.Fatalf("retain result: %v", err)
			}
			if failure == "member" && barriers != 0 {
				t.Fatal("published after member failure")
			}
			wantBarriers := 2
			if failure == "member" {
				wantBarriers = 0
			}
			if failure == "data barrier" || failure == "canceled before publication" {
				wantBarriers = 1
			}
			if barriers != wantBarriers {
				t.Fatalf("full flushes = %d, want %d", barriers, wantBarriers)
			}
			if failure == "member" || failure == "data barrier" || failure == "canceled before publication" {
				entries, err := os.ReadDir(store.Directory)
				if err != nil || len(entries) != 0 {
					t.Fatalf("failed preparation left bodies: %v, %v", entries, err)
				}
			}
			// An interrupted publication must be safely completable from verified blobs.
			if failure == "publication" {
				if err := os.RemoveAll(root); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Retain(context.Background(), root, m); err != nil {
				t.Fatal(err)
			}
			assertTransferBlobsComplete(t, store.Directory)
			barriers = 0
			err = store.retain(context.Background(), "/unavailable", m, func(*os.File) error { t.Error("rewrote verified cached body"); return nil }, func(root *os.Root) error { barriers++; return syncApplyRootDirectory(root, ".") })
			if err != nil || barriers != 1 {
				t.Fatalf("cached retry omitted publication barrier: %d, %v", barriers, err)
			}
		})
	}
}

func TestOrdinaryRestoreRestrictsRootAfterPunctuationChild(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "!dir")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(root, 0700); os.Chmod(child, 0700) })
	if err := os.Chmod(child, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	access, err := makeTreeAccessible(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := access.restore(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("root mode: %v, %v", info, err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(child)
	if err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("child mode: %v, %v", info, err)
	}
}
