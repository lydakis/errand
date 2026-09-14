package changes

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestMaterializeMergeInputPreservesRestrictedSource(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		name := "valid"
		if corrupt {
			name = "corrupt"
		}
		t.Run(name, func(t *testing.T) {
			source, dest := t.TempDir(), t.TempDir()
			t.Cleanup(func() { _ = RemoveTree(source); _ = RemoveTree(dest) })
			if err := os.Mkdir(filepath.Join(source, "dir"), 0700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(source, "dir/file")
			if err := os.WriteFile(file, []byte("before"), 0600); err != nil {
				t.Fatal(err)
			}
			m, err := snapshot.Build(source, []string{"dir", "dir/file"})
			if err != nil {
				t.Fatal(err)
			}
			if corrupt {
				if err := os.WriteFile(file, []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for i := len(m.Entries) - 1; i >= 0; i-- {
				m.Entries[i].Mode = 0
				if err := os.Chmod(filepath.Join(source, m.Entries[i].Path), 0); err != nil {
					t.Fatal(err)
				}
			}
			access, err := materializeMergeInput(source, dest, m)
			if corrupt && err == nil || !corrupt && err != nil {
				t.Fatalf("materialization: %v", err)
			}
			for _, entry := range m.Entries {
				path := filepath.Join(source, entry.Path)
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0 {
					t.Fatalf("source mode not restored: %s, %v", entry.Path, err)
				}
				if entry.Type == proto.EntryDir {
					if err := os.Chmod(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			if corrupt {
				return
			}
			defer access.closeWithoutRestore()
			if err := snapshot.PackContextWithPhysicalModes(t.Context(), io.Discard, dest, m, access.physical); err != nil {
				t.Fatal(err)
			}
			if access.original["dir/file"] != 0 {
				t.Fatal("lost logical file mode")
			}
			if err := os.Chmod(file, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte("edited"), 0600); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(filepath.Join(dest, "dir/file"))
			if err != nil || string(body) != "before" {
				t.Fatalf("source edit changed merge input: %q, %v", body, err)
			}
		})
	}
}
