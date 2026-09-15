package changes

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

// Deliberately synthesize the clone result so non-CoW CI verifies this branch.
type captureCloneReader struct {
	io.ReadCloser
	body string
}

func (r captureCloneReader) cloneTo(root *os.Root, name string) (*os.File, error) {
	if err := root.WriteFile(name, []byte(r.body), 0600); err != nil {
		return nil, err
	}
	return root.Open(name)
}

func TestCaptureVerifiesClonedResultBeforeSync(t *testing.T) {
	for _, body := range []string{"good", "evil"} {
		t.Run(body, func(t *testing.T) {
			tree, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer tree.Close()
			synced, published := false, false
			m := proto.Manifest{Entries: []proto.ManifestEntry{{Path: "file", Type: proto.EntryFile, Mode: 0600, Size: 4, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("good")))}}}
			policy := materializationPolicy{cloneFiles: true, syncData: func(*os.File) error { synced = true; return nil }, barrier: func() error { published = true; return nil }}
			err = materializeTransferTree(t.Context(), tree, m, policy, func(proto.ManifestEntry) (io.ReadCloser, error) {
				return captureCloneReader{io.NopCloser(strings.NewReader("good")), body}, nil
			})
			if body == "evil" {
				if err == nil || synced || published {
					t.Fatalf("accepted corrupt clone: %v, sync=%v, publish=%v", err, synced, published)
				}
			} else if err != nil || !synced || !published {
				t.Fatalf("rejected valid clone: %v, sync=%v, publish=%v", err, synced, published)
			}
		})
	}
}

func TestCaptureBaseRejectsCorruptionBeforeSync(t *testing.T) {
	source, job := t.TempDir(), t.TempDir()
	file := filepath.Join(source, "file")
	if err := os.WriteFile(file, []byte("good"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := snapshot.Build(source, []string{"file"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("evil"), 0600); err != nil {
		t.Fatal(err)
	}
	err = captureWorkspaceBaseContext(t.Context(), source, job, m,
		func(*os.File) error { t.Error("synced corrupt captured content"); return nil },
		func(string) error { t.Error("published corrupt captured content"); return nil })
	if err == nil {
		t.Fatal("accepted same-size corruption")
	}
	entries, err := os.ReadDir(job)
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial capture: %v, %v", entries, err)
	}
}

func TestCaptureBasePreservesReadOnlySource(t *testing.T) {
	source, job := t.TempDir(), t.TempDir()
	file := filepath.Join(source, "file")
	if err := os.WriteFile(file, []byte("body"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0400); err != nil {
		t.Fatal(err)
	}
	m, err := snapshot.Build(source, []string{"file"})
	if err != nil {
		t.Fatal(err)
	}
	if err := CaptureWorkspaceBaseContext(t.Context(), source, job, m); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatalf("source mode changed: %v, %v", info, err)
	}
	if err := snapshot.PackContext(context.Background(), io.Discard, workspaceBasePath(job), m); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureBasePreservesSearchOnlyDirectories(t *testing.T) {
	for _, name := range []string{"Dir/file", "Dir/nested/deep/file", "dir/file"} {
		t.Run(name, func(t *testing.T) {
			source, job := t.TempDir(), t.TempDir()
			if err := os.MkdirAll(filepath.Join(source, "Dir/nested/deep"), 0700); err != nil {
				t.Fatal(err)
			}
			if name == "dir/file" {
				if _, err := os.Stat(filepath.Join(source, "dir")); err != nil {
					t.Skip("case sensitive")
				}
			}
			if err := os.WriteFile(filepath.Join(source, name), []byte("body"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("file", filepath.Join(source, "Dir/link")); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(source, "Dir"), 0100); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(filepath.Join(source, "Dir"), 0700) })
			m, err := snapshot.Build(source, []string{"Dir", name, "Dir/link"})
			if err != nil {
				t.Fatal(err)
			}
			// Exercise an implicit spelling as well as ordinary and cached parents.
			if name == "dir/file" {
				entries := []proto.ManifestEntry{}
				for _, e := range m.Entries {
					if e.Path != "dir" {
						entries = append(entries, e)
					}
				}
				m.Entries = entries
			}
			if err := CaptureWorkspaceBaseContext(t.Context(), source, job, m); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { RemoveTree(workspaceBasePath(job)) })
			if info, err := os.Stat(filepath.Join(source, "Dir")); err != nil || info.Mode().Perm() != 0100 {
				t.Fatalf("changed source permissions: %v, %v", info, err)
			}
			if info, err := os.Stat(filepath.Join(workspaceBasePath(job), "Dir")); err != nil || info.Mode().Perm() != 0100 {
				t.Fatalf("incorrect captured permissions: %v, %v", info, err)
			}
			// The oracle needs readable directories; keep the logical modes in m.
			access, err := makeManifestAccessibleContext(t.Context(), workspaceBasePath(job), m)
			if err != nil {
				t.Fatal(err)
			}
			defer access.restore()
			if err := snapshot.PackContextWithPhysicalModes(t.Context(), io.Discard, workspaceBasePath(job), m, access.physical); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCaptureCopyFallbackPreservesIndependentBody(t *testing.T) {
	source, dest := t.TempDir(), t.TempDir()
	file := filepath.Join(source, "file")
	if err := os.WriteFile(file, []byte("body"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := snapshot.Build(source, []string{"file"})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := os.OpenRoot(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	policy := durableMaterialization(func() error { return nil })
	policy.cloneFiles = true
	// A reader without clone support takes the fallback used on non-CoW filesystems.
	err = materializeTransferTree(t.Context(), tree, m, policy, func(e proto.ManifestEntry) (io.ReadCloser, error) {
		f, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		return struct{ io.ReadCloser }{f}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.PackContext(t.Context(), io.Discard, dest, m); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureBasePreservesImplicitAliasMode(t *testing.T) {
	source, job := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "Dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(source, "dir")); err != nil {
		t.Skip("case sensitive")
	}
	if err := os.WriteFile(filepath.Join(source, "Dir/file"), []byte("body"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "Dir"), 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(source, "Dir"), 0700) })
	m, err := snapshot.Build(source, []string{"Dir", "dir/file"})
	if err != nil {
		t.Fatal(err)
	}
	entries := []proto.ManifestEntry{}
	for _, e := range m.Entries {
		if e.Path != "dir" {
			entries = append(entries, e)
		}
	}
	m.Entries = entries
	if err := CaptureWorkspaceBaseContext(t.Context(), source, job, m); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { RemoveTree(workspaceBasePath(job)) })
	if err := snapshot.PackContext(t.Context(), io.Discard, workspaceBasePath(job), m); err != nil {
		t.Fatal(err)
	}
}
