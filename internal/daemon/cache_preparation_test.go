package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func TestConcurrentAdmissionsRestoreMissingCacheBeforeUse(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d, ts := concurrencyDaemon(t, 2, 2)
	root := workspaceWith(t, map[string]string{".gitignore": "cache/\n"})
	opts := client.RunOptions{PeerURL: ts.URL, Root: root, Caches: []proto.CacheBinding{{Name: "deps", Path: "cache"}}}
	ws, err := client.CreateWorkspace(opts, "repair")
	if err != nil {
		t.Fatal(err)
	}
	id := submitWorkspaceCommand(t, ts.URL, root, ws, "printf saved > cache/value")
	requireCacheJobSuccess(t, ts.URL, id)
	cache := filepath.Join(d.workspaces.dir, ws.ID, "data/cache")
	if err := os.RemoveAll(cache); err != nil {
		t.Fatal(err)
	}
	acquire := func() *Job {
		t.Helper()
		j := newJob(proto.NewULID(), t.TempDir())
		j.Spec = proto.Spec{WorkspaceID: ws.ID, CacheProjectID: ws.CacheProjectID, ManifestRoot: ws.Manifest.RootHash(), Selection: ws.Selection}
		if err := d.acquireWorkspace(t.Context(), j); err != nil {
			t.Fatal(err)
		}
		return j
	}
	// Admission and preparation take separate workspace locks. Both members
	// can be registered before either prepares a directory for command use.
	first, second := acquire(), acquire()
	if err := first.prepareNamedCacheTrees(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(cache, "value")); err != nil || string(raw) != "saved" {
		t.Fatalf("concurrent admissions skipped restoration: %q %v", raw, err)
	}
	// Once a prepared member can run, a sibling must not undo its deletion
	// or interfere while it is replacing the directory.
	if err := os.RemoveAll(cache); err != nil {
		t.Fatal(err)
	}
	if err := second.prepareNamedCacheTrees(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(cache); !os.IsNotExist(err) {
		t.Fatalf("restored over a prepared member's deletion: %v", err)
	}
	for _, j := range []*Job{first, second} {
		if err := d.returnWorkspace(j); err != nil {
			t.Fatal(err)
		}
	}
	// A later independent admission must be able to repair the missing
	// directory again after all previous members have settled.
	next := acquire()
	if err := next.prepareNamedCacheTrees(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(cache, "value")); err != nil || string(raw) != "saved" {
		t.Fatalf("later admission skipped restoration: %q %v", raw, err)
	}
	if err := d.returnWorkspace(next); err != nil {
		t.Fatal(err)
	}
}
