package changes

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestExportRemoteSelectionPreservesPathsAndModes(t *testing.T) {
	workspace, job := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"dist/tool": "binary", "other": "omit"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("tool", filepath.Join(workspace, "dist/link")); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := collectTestChanges(workspace, job, []testChangeRoot{{"dist"}, {"other"}}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	staged := extractTestBundle(t, job, bundle)
	for _, selection := range []string{"", "dist", "dist/tool"} {
		t.Run(selection, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "export")
			if err := ExportRemote(staged, dest, selection, bundle); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(dest, "dist/tool"))
			if err != nil || string(got) != "binary" {
				t.Fatalf("export: %q %v", got, err)
			}
			info, err := os.Stat(filepath.Join(dest, "dist/tool"))
			if err != nil || info.Mode().Perm() != 0o755 {
				t.Fatalf("mode: %v %v", info, err)
			}
			if selection != "" {
				if _, err := os.Lstat(filepath.Join(dest, "other")); !os.IsNotExist(err) {
					t.Fatalf("unselected file exported: %v", err)
				}
			}
			if selection != "dist/tool" {
				if link, err := os.Readlink(filepath.Join(dest, "dist/link")); err != nil || link != "tool" {
					t.Fatalf("symlink: %q %v", link, err)
				}
			}
		})
	}
}

func TestExportRemoteRefusesDeletedSelection(t *testing.T) {
	workspace, job := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(workspace, "nested/deleted")
	if err := os.WriteFile(file, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshot.Build(workspace, []string{"nested", "nested/deleted"})
	if err != nil {
		t.Fatal(err)
	}
	if err := CaptureWorkspaceBaseContext(context.Background(), workspace, job, manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := CollectWorkspaceChangesContext(context.Background(), workspace, job, manifest, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	staged := extractTestBundle(t, job, bundle)
	for _, selected := range []string{"", "nested", "nested/deleted", "unknown", "../deleted"} {
		dest := filepath.Join(t.TempDir(), "output")
		if err := ExportRemote(staged, dest, selected, bundle); err == nil {
			t.Fatalf("exported selection %q with no remote value", selected)
		}
		if _, err := os.Lstat(dest); !os.IsNotExist(err) {
			t.Fatalf("created destination for %q: %v", selected, err)
		}
	}
}

func TestExportRemoteRefusesExistingDestinationAndCorruptContent(t *testing.T) {
	workspace, job := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "result"), []byte("valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := collectTestChanges(workspace, job, []testChangeRoot{{"result"}}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	staged := extractTestBundle(t, job, bundle)
	parent := t.TempDir()
	existing := filepath.Join(parent, "existing")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ExportRemote(staged, existing, "", bundle); err == nil {
		t.Fatal("replaced existing directory")
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(existing, link); err != nil {
		t.Fatal(err)
	}
	if err := ExportRemote(staged, link, "", bundle); err == nil {
		t.Fatal("replaced destination symlink")
	}
	if err := os.WriteFile(filepath.Join(staged, "remote/result"), []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(parent, "output")
	if err := ExportRemote(staged, dest, "", bundle); err == nil {
		t.Fatal("exported corrupt content")
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("failed export published output: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 2 {
		t.Fatalf("export left temporary files: %v %v", entries, err)
	}
}
