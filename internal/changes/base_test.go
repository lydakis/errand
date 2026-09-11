package changes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func captureFixture(t *testing.T) (string, proto.Manifest) {
	t.Helper()
	root := t.TempDir()
	var paths []string
	for i := range 64 {
		name := fmt.Sprintf("dir-%d/file-%02d", i%4, i)
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o700); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}
	if err := os.Symlink("dir-0/file-00", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshot.Build(root, append(paths, "link"))
	if err != nil {
		t.Fatal(err)
	}
	return root, manifest
}

func TestCaptureBasePreservesIndependentTree(t *testing.T) {
	root, manifest := captureFixture(t)
	job := t.TempDir()
	if err := CaptureWorkspaceBaseContext(context.Background(), root, job, manifest); err != nil {
		t.Fatal(err)
	}
	for _, entry := range manifest.Entries {
		if entry.Type == proto.EntryFile {
			if err := os.WriteFile(filepath.Join(root, entry.Path), []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Validate every captured byte, mode, directory, and symlink after the live
	// workspace changes. In particular, hard links cannot replace file clones.
	if err := snapshot.PackContext(context.Background(), io.Discard, workspaceBasePath(job), manifest); err != nil {
		t.Fatal(err)
	}
}

func TestCloneOrCopyFileFallback(t *testing.T) {
	for _, failure := range []bool{false, true} {
		name := "success"
		if failure {
			name = "sync-failure"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			src, dest := filepath.Join(root, "source"), filepath.Join(root, "captured")
			const content = "original contents"
			if err := os.WriteFile(src, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			// Both clone implementations require an absent destination. An
			// existing placeholder forces the byte-copy fallback on any filesystem.
			if err := os.WriteFile(dest, []byte("placeholder"), 0o600); err != nil {
				t.Fatal(err)
			}
			const mode = os.FileMode(0o500)
			injected := errors.New("injected sync failure")
			err := cloneOrCopyFile(context.Background(), src, dest, mode, func(file *os.File) error {
				if failure {
					return injected
				}
				return syncCapturedData(file)
			})
			if failure {
				if !errors.Is(err, injected) {
					t.Fatalf("copy error = %v, want sync failure", err)
				}
				if _, err := os.Stat(dest); !os.IsNotExist(err) {
					t.Fatalf("failed copy left destination: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(src, []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(dest); err != nil || string(got) != content {
				t.Fatalf("captured contents = %q, %v", got, err)
			}
			info, err := os.Stat(dest)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != mode {
				t.Fatalf("captured mode = %o, want %o", got, mode)
			}
		})
	}
}

func TestCaptureBaseFailureLeavesNoPartialTree(t *testing.T) {
	for _, failure := range []string{"missing-file", "changed-content", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			root, manifest := captureFixture(t)
			job := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch failure {
			case "missing-file":
				if err := os.Remove(filepath.Join(root, "dir-2/file-62")); err != nil {
					t.Fatal(err)
				}
			case "changed-content":
				if err := os.WriteFile(filepath.Join(root, "dir-2/file-62"), []byte("changed"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			err := captureWorkspaceBaseContext(ctx, root, job, manifest, func(file *os.File) error {
				if failure == "cancelled" {
					cancel()
				}
				return syncCapturedData(file)
			}, syncDirectory)
			if err == nil {
				t.Fatal("capture succeeded despite failure")
			}
			if failure == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("capture error = %v, want cancellation", err)
			}
			entries, err := os.ReadDir(job)
			if err != nil || len(entries) != 0 {
				t.Fatalf("capture left partial state: %v, %v", entries, err)
			}
		})
	}
}

func TestCaptureBaseDurabilityBeforePublication(t *testing.T) {
	for _, failure := range []string{"none", "member", "tree", "publication"} {
		t.Run(failure, func(t *testing.T) {
			root, manifest := captureFixture(t)
			job := t.TempDir()
			wantSynced := int32(0)
			for _, entry := range manifest.Entries {
				if entry.Type != proto.EntrySymlink {
					wantSynced++
				}
			}
			var synced atomic.Int32
			injected := errors.New("injected sync failure")
			barriers := 0
			err := captureWorkspaceBaseContext(context.Background(), root, job, manifest,
				func(file *os.File) error {
					if failure == "member" {
						return injected
					}
					if err := syncCapturedData(file); err != nil {
						return err
					}
					synced.Add(1)
					return nil
				}, func(path string) error {
					barriers++
					if got := synced.Load(); got != wantSynced {
						t.Errorf("publication barrier reached after syncing %d of %d members", got, wantSynced)
					}
					if barriers == 1 {
						if _, err := os.Stat(workspaceBasePath(job)); !os.IsNotExist(err) {
							t.Errorf("base published before full tree sync: %v", err)
						}
						if err := snapshot.PackContext(context.Background(), io.Discard, path, manifest); err != nil {
							t.Errorf("tree incomplete at durability barrier: %v", err)
						}
						if failure == "tree" {
							return injected
						}
					} else {
						if path != job {
							t.Errorf("publication sync path = %q, want %q", path, job)
						}
						if err := snapshot.PackContext(context.Background(), io.Discard, workspaceBasePath(job), manifest); err != nil {
							t.Errorf("published base incomplete: %v", err)
						}
						if failure == "publication" {
							return injected
						}
					}
					return syncDirectory(path)
				})
			if failure == "none" && err != nil || failure != "none" && !errors.Is(err, injected) {
				t.Fatalf("capture error = %v, failure = %s", err, failure)
			}
			wantBarriers := map[string]int{"none": 2, "member": 0, "tree": 1, "publication": 2}[failure]
			if barriers != wantBarriers {
				t.Fatalf("full syncs = %d, want %d", barriers, wantBarriers)
			}
			entries, err := os.ReadDir(job)
			if err != nil {
				t.Fatal(err)
			}
			wantEntries := 0
			if failure == "none" || failure == "publication" {
				wantEntries = 1
			}
			if len(entries) != wantEntries {
				t.Fatalf("unexpected state after capture: %v", entries)
			}
		})
	}
}
