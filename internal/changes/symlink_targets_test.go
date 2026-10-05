package changes

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestSymlinkTargetsCollectOnWindows(t *testing.T) {
	ctx := context.Background()
	workspace, job := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"file", "other"} {
		if err := os.WriteFile(filepath.Join(workspace, "src", name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	targets := map[string]string{"link": "src/file", "src/link": "../src/file", "dangling": "missing/file"}
	if runtime.GOOS != "windows" {
		targets["literal"] = `src\file`
	}
	paths := []string{"src", "src/file", "src/other"}
	for name, target := range targets {
		if err := os.Symlink(target, filepath.Join(workspace, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}
	baseline, err := snapshot.Build(workspace, paths)
	if err != nil {
		t.Fatal(err)
	}
	// Use the Unix sender's target spelling even on Windows.
	for i := range baseline.Entries {
		if target, ok := targets[baseline.Entries[i].Path]; ok {
			baseline.Entries[i].Target = target
		}
	}
	if err := CaptureJobBaseContext(ctx, workspace, job, baseline, nil); err != nil {
		t.Fatal(err)
	}
	if bundle, collected, err := CollectWorkspaceChangesContext(ctx, workspace, job, nil, baseline, proto.SelectionPolicy{}, 1<<20); err != nil || collected {
		t.Fatalf("unchanged links: collected = %t, bundle = %+v, err = %v", collected, bundle, err)
	}
	if err := os.Remove(filepath.Join(workspace, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("src/other", filepath.Join(workspace, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../src/file", filepath.Join(workspace, "src", "new-link")); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := CollectWorkspaceChangesContext(ctx, workspace, job, nil, baseline, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !collected || !reflect.DeepEqual(bundle.Paths, []string{"link", "src/new-link"}) {
		t.Fatalf("changed links: collected = %t, paths = %v", collected, bundle.Paths)
	}
	for _, e := range bundle.RemoteManifest.Entries {
		if e.Path == "link" && e.Target != "src/other" || e.Path == "src/new-link" && e.Target != "../src/file" {
			t.Fatalf("remote target for %s = %q", e.Path, e.Target)
		}
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	captured, _, _, _, err := captureManifestAtRootBoundedContext(ctx, root, "src/new-link", "src/new-link", -1, -1)
	if err != nil || len(captured.Entries) != 1 || captured.Entries[0].Target != "../src/file" {
		t.Fatalf("captured link = %+v, err = %v", captured, err)
	}
	staged := extractTestBundle(t, job, bundle)
	if err := VerifyExtracted(staged, bundle); err != nil {
		t.Fatal(err)
	}
}
