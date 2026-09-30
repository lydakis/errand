package changes

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/fsmode"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

// A Windows runner receives modes from a Unix client but can't store them.
// Results must keep them and report only what the job changed.
func TestCollectionKeepsSubmittedModesOnWindows(t *testing.T) {
	if !fsmode.Logical {
		t.Skip("file system stores POSIX modes")
	}
	workspace := t.TempDir()
	jobDir := t.TempDir()
	files := map[string]string{"run.sh": "echo hi\n", "lib.txt": "v1\n", "bin/tool": "tool\n", "locked.txt": "keep\n"}
	if err := os.Mkdir(filepath.Join(workspace, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(workspace, filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	locked := filepath.Join(workspace, "locked.txt")
	if err := os.Chmod(locked, 0o444); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o644)
	baseline, err := snapshot.Build(workspace, []string{"bin", "bin/tool", "lib.txt", "locked.txt", "run.sh"})
	if err != nil {
		t.Fatal(err)
	}
	submitted := map[string]uint32{"bin": 0o755, "bin/tool": 0o755, "lib.txt": 0o644, "locked.txt": 0o444, "run.sh": 0o755}
	for i := range baseline.Entries {
		baseline.Entries[i].Mode = submitted[baseline.Entries[i].Path]
	}
	if err := CaptureWorkspaceBaseContext(context.Background(), workspace, jobDir, baseline); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(workspace, "lib.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "bin", "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := CollectWorkspaceChangesContext(context.Background(), workspace, jobDir, baseline, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !collected || fmt.Sprint(bundle.Paths) != "[bin/new.txt lib.txt]" {
		t.Fatalf("collected = %t, paths = %v", collected, bundle.Paths)
	}
	want := map[string]uint32{"bin": 0o755, "bin/new.txt": 0o644, "lib.txt": 0o644}
	for _, entry := range bundle.RemoteManifest.Entries {
		if mode, ok := want[entry.Path]; ok && entry.Mode != mode {
			t.Fatalf("remote mode for %s = %#o, want %#o", entry.Path, entry.Mode, mode)
		}
	}
	staged := extractTestBundle(t, jobDir, bundle)
	if err := VerifyExtracted(staged, bundle); err != nil {
		t.Fatal(err)
	}
}
