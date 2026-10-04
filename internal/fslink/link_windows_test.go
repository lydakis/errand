//go:build windows

package fslink

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTypedSymlinkWithoutReferentOnWindows(t *testing.T) {
	for _, directory := range []bool{false, true} {
		name := "file"
		if directory {
			name = "directory"
		}
		t.Run(name, func(t *testing.T) {
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := root.Mkdir("private", 0o700); err != nil {
				t.Fatal(err)
			}
			if err := Create(root, "target 世界", "private/value", directory); err != nil {
				t.Fatal(err)
			}
			info, err := root.Lstat("private/value")
			if err != nil || info.Mode()&os.ModeSymlink == 0 || Directory(info) != directory {
				t.Fatalf("staged link type: %v, %v", info, err)
			}
			if err := root.Rename("private/value", "alias"); err != nil {
				t.Fatal(err)
			}
			target := "target 世界"
			if directory {
				if err := root.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
				target += "/value"
			}
			if err := root.WriteFile(target, []byte("body"), 0o600); err != nil {
				t.Fatal(err)
			}
			alias := "alias"
			if directory {
				alias += "/value"
			}
			if body, err := root.ReadFile(alias); err != nil || string(body) != "body" {
				t.Fatalf("installed link: %q, %v", body, err)
			}
			if err := Create(root, "other", "alias", directory); err == nil {
				t.Fatal("replaced an existing link")
			}
		})
	}
}

func TestTypedSymlinkKeepsRootBoundaryOnWindows(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{"../outside", "escape/link"} {
		if err := Create(root, "target", name, true); err == nil {
			t.Fatalf("created link outside the root through %q", name)
		}
	}
	if _, err := os.Lstat(filepath.Join(outside, "link")); !os.IsNotExist(err) {
		t.Fatalf("outside path changed: %v", err)
	}
}
