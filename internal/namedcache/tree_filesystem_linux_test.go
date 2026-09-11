//go:build linux

package namedcache

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestTreeCrossFilesystemRestoreAndPublication(t *testing.T) {
	workspace, err := os.MkdirTemp("/dev/shm", "errand-cache-fs-")
	if err != nil {
		t.Skipf("second filesystem unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	source := t.TempDir()
	a, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if a.Sys().(*syscall.Stat_t).Dev == b.Sys().(*syscall.Stat_t).Dev {
		t.Skip("fixture directories share a filesystem")
	}
	s := openTestStore(t, t.TempDir(), 1<<20)
	key, id := Key{"owner", "project", "cross-filesystem"}, proto.NewULID()
	if _, err := s.AcquireTree(t.Context(), key, id); err != nil {
		t.Fatal(err)
	}
	base, err := s.RestoreTree(t.Context(), key, id, source, "cache")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "cache/value"), []byte("original"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishTree(t.Context(), key, id, source, "cache", base, false); err != nil {
		t.Fatal(err)
	}
	base, err = s.RestoreTree(t.Context(), key, id, workspace, "cache")
	if err != nil || !strings.HasPrefix(base, "private:") {
		t.Fatalf("cross-filesystem mode: %q %v", base, err)
	}
	actual, err := TreeFingerprint(t.Context(), workspace, "cache", base)
	if err != nil || actual != base {
		t.Fatalf("fallback metadata: %q %q %v", base, actual, err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "cache/value"), []byte("changed on another filesystem"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishTree(t.Context(), key, id, workspace, "cache", base, true); err != nil {
		t.Fatal(err)
	}
	restored := t.TempDir()
	if _, err := s.RestoreTree(t.Context(), key, id, restored, "cache"); err != nil {
		t.Fatal(err)
	}
	value, err := os.ReadFile(filepath.Join(restored, "cache/value"))
	if err != nil || string(value) != "changed on another filesystem" {
		t.Fatalf("cross-filesystem publication: %q %v", value, err)
	}
}

func TestColdTreeCrossFilesystemPublication(t *testing.T) {
	for _, tc := range []struct {
		name            string
		consume, legacy bool
	}{
		{name: "persistent"},
		{name: "ephemeral", consume: true},
		{name: "legacy-linked", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace, err := os.MkdirTemp("/dev/shm", "errand-cache-cold-")
			if err != nil {
				t.Skipf("second filesystem unavailable: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(workspace) })
			storeRoot := t.TempDir()
			a, err := os.Stat(storeRoot)
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.Stat(workspace)
			if err != nil {
				t.Fatal(err)
			}
			if a.Sys().(*syscall.Stat_t).Dev == b.Sys().(*syscall.Stat_t).Dev {
				t.Skip("fixture directories share a filesystem")
			}
			s := openTestStore(t, storeRoot, 1<<20)
			key, id := Key{"owner", "project", "cold-cross-filesystem"}, proto.NewULID()
			if _, err := s.AcquireTree(t.Context(), key, id); err != nil {
				t.Fatal(err)
			}
			base, err := s.RestoreTree(t.Context(), key, id, workspace, "cache")
			if err != nil {
				t.Fatal(err)
			}
			if tc.legacy {
				// Earlier receipts classified an empty tree as linked. Keep that
				// valid baseline to exercise publication's real EXDEV fallback.
				base = strings.Replace(base, "private:", "links:", 1)
			}
			file := filepath.Join(workspace, "cache/value")
			if err := os.WriteFile(file, []byte("installed"), 0600); err != nil {
				t.Fatal(err)
			}
			base, err = s.PublishTree(t.Context(), key, id, workspace, "cache", base, tc.consume)
			if err != nil {
				t.Fatalf("first installation could not be published: %v", err)
			}
			mode := "private:"
			if tc.legacy {
				mode = "links:"
			}
			if !strings.HasPrefix(base, mode) {
				t.Fatalf("workspace comparison mode changed: %q", base)
			}
			actual, err := TreeFingerprint(t.Context(), workspace, "cache", base)
			if err != nil || actual != base {
				t.Fatalf("publication returned the wrong workspace baseline: %q %q %v", base, actual, err)
			}
			if !tc.consume {
				// A persistent workspace must detect later writes without another restore.
				updated := file
				if tc.legacy {
					updated += ".new" // Linked views retain their atomic-replacement contract.
				}
				if err := os.WriteFile(updated, []byte("updated installation"), 0600); err != nil {
					t.Fatal(err)
				}
				if updated != file {
					if err := os.Rename(updated, file); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := s.PublishTree(t.Context(), key, id, workspace, "cache", base, false); err != nil {
					t.Fatal(err)
				}
			}
			restored := t.TempDir()
			if _, err := s.RestoreTree(t.Context(), key, id, restored, "cache"); err != nil {
				t.Fatal(err)
			}
			want := "installed"
			if !tc.consume {
				want = "updated installation"
			}
			if value, err := os.ReadFile(filepath.Join(restored, "cache/value")); err != nil || string(value) != want {
				t.Fatalf("publication contents: %q %v", value, err)
			}
		})
	}
}
