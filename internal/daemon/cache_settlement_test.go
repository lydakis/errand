package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func TestReplacedCacheParentReleasesEphemeralHolder(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d, ts := concurrencyDaemon(t, 2, 1)
	root := workspaceWith(t, map[string]string{".gitignore": "pkg/cache/\n"})
	var out bytes.Buffer
	opts := client.RunOptions{
		PeerURL: ts.URL, Root: root, Caches: []proto.CacheBinding{{Name: "deps", Path: "pkg/cache"}},
		Argv: []string{"/bin/sh", "-c", "rm -rf pkg; printf replacement > pkg"}, Stdout: &out, Stderr: &out,
	}
	if code := client.Run(opts); code == 0 {
		t.Fatal("replaced parent should still report the collection/publication error")
	}
	rows, err := client.List(ts.URL)
	if err != nil || len(rows) != 1 {
		t.Fatalf("jobs: %+v %v", rows, err)
	}
	result := waitTerminal(t, ts.URL, rows[0].ID).Result
	if result == nil || result.ExitCode == nil || *result.ExitCode != 0 || !result.CleanupOK || !strings.Contains(result.TransactionError, "cache parent") {
		t.Fatalf("stopped job failed runtime cleanup: %+v\n%s", result, &out)
	}
	entries, err := d.namedCaches.Inventory(t.Context())
	if err != nil || len(entries) != 1 || entries[0].Protected() {
		t.Fatalf("finished job retained cache holder: %+v %v", entries, err)
	}
	if _, err := os.Lstat(filepath.Join(d.jobsDir(), rows[0].ID, "workspace")); !os.IsNotExist(err) {
		t.Fatalf("finished job retained workspace: %v", err)
	}
}

func TestCachePublicationFailureReleasesFinishedWorkspaceMember(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d, ts := concurrencyDaemon(t, 2, 1)
	root := workspaceWith(t, map[string]string{".gitignore": "cache/\n"})
	opts := client.RunOptions{PeerURL: ts.URL, Root: root, Caches: []proto.CacheBinding{{Name: "deps", Path: "cache"}}}
	ws, err := client.CreateWorkspace(opts, "repair")
	if err != nil {
		t.Fatal(err)
	}
	first := submitWorkspaceCommand(t, ts.URL, root, ws, "mkfifo cache/pipe")
	result := waitTerminal(t, ts.URL, first).Result
	if result == nil || result.ExitCode == nil || *result.ExitCode != 0 || !result.CleanupOK || !strings.Contains(result.TransactionError, "unsupported installed cache entry") {
		t.Fatalf("publication should report failure after releasing runtime state: %+v", result)
	}
	second := submitWorkspaceCommand(t, ts.URL, root, ws, "rm cache/pipe; printf repaired > cache/value")
	requireCacheJobSuccess(t, ts.URL, second)
	entries, err := d.namedCaches.Inventory(t.Context())
	if err != nil || len(entries) != 1 || entries[0].Protected() {
		t.Fatalf("retained cache holder: %+v %v", entries, err)
	}
	// A durable receipt can outlive its directory after a host failure.
	if err := os.RemoveAll(filepath.Join(d.workspaces.dir, ws.ID, "data/cache")); err != nil {
		t.Fatal(err)
	}
	restored := submitWorkspaceCommand(t, ts.URL, root, ws, "test \"$(cat cache/value)\" = repaired")
	requireCacheJobSuccess(t, ts.URL, restored)
	if err := client.RemoveWorkspace(ts.URL, ws.Name); err != nil {
		t.Fatal(err)
	}
}
