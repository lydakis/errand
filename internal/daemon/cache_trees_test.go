package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/namedcache"
	"github.com/lydakis/errand/internal/proto"
)

func requireCacheJobSuccess(t *testing.T, url, id string) {
	t.Helper()
	r := waitTerminal(t, url, id).Result
	if r == nil || !r.Started || r.ExitCode == nil || *r.ExitCode != 0 || !r.CleanupOK || !r.ChangesOK {
		t.Fatalf("cache job: %+v", r)
	}
}

func TestCacheTreeConcurrentWorkspacePublication(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d, ts := concurrencyDaemon(t, 3, 2)
	root := workspaceWith(t, map[string]string{".gitignore": "node_modules/\n"})
	opts := client.RunOptions{PeerURL: ts.URL, Root: root, Caches: []proto.CacheBinding{{Name: "deps", Path: "node_modules"}}}
	run := func(command string) {
		t.Helper()
		var out bytes.Buffer
		opts.Argv, opts.Stdout, opts.Stderr = []string{"/bin/sh", "-c", command}, &out, &out
		if code := client.Run(opts); code != 0 {
			t.Fatalf("%s: %d\n%s", command, code, &out)
		}
	}
	run("printf original > node_modules/.state")
	ws, err := client.CreateWorkspace(opts, "shared")
	if err != nil {
		t.Fatal(err)
	}
	reader := submitWorkspaceCommand(t, ts.URL, root, ws, "while test ! -e finish; do sleep 0.02; done; test -d node_modules")
	waitState(t, ts.URL, reader, proto.StateRunning)
	writer := submitWorkspaceCommand(t, ts.URL, root, ws, "printf updated > node_modules/.state")
	requireCacheJobSuccess(t, ts.URL, writer)
	// The writer's exit leaves the shared tree live for its sibling. Publish
	// only when the last member returns, even though that member is a reader.
	waitState(t, ts.URL, reader, proto.StateRunning)
	if err := os.WriteFile(filepath.Join(d.workspaces.dir, ws.ID, "data/finish"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	requireCacheJobSuccess(t, ts.URL, reader)
	run("test \"$(cat node_modules/.state)\" = updated")
	// Failed writes do not replace the last successful saved layout.
	opts.Argv = []string{"/bin/sh", "-c", "printf failed > node_modules/.new; mv node_modules/.new node_modules/.state; exit 17"}
	opts.Stdout, opts.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	if code := client.Run(opts); code != 17 {
		t.Fatalf("failed command exit: %d", code)
	}
	run("test \"$(cat node_modules/.state)\" = updated")
	entries, err := d.namedCaches.Inventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Protected() {
			t.Fatalf("leaked tree holder: %+v", entry)
		}
	}
	// The persistent workspace also retains its live directory between jobs.
	opts.Workspace = ws.Name
	run("test \"$(cat node_modules/.state)\" = updated")
}

func TestCacheTreeEphemeralJobsOverlap(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d, ts := concurrencyDaemon(t, 2, 1)
	root := workspaceWith(t, map[string]string{".gitignore": "node_modules/\n"})
	opts := client.RunOptions{PeerURL: ts.URL, Root: root, Caches: []proto.CacheBinding{{Name: "deps", Path: "node_modules"}}, Argv: []string{"/bin/sh", "-c", "test -d node_modules; sleep 60"}}
	opts.Detach = true
	var out bytes.Buffer
	opts.Stdout, opts.Stderr = &out, &out
	for range 2 {
		if code := client.Run(opts); code != 0 {
			t.Fatalf("submission: %d %s", code, &out)
		}
	}
	rows, err := client.List(ts.URL)
	if err != nil || len(rows) != 2 {
		t.Fatalf("jobs: %v %v", rows, err)
	}
	for _, row := range rows {
		t.Cleanup(func() { _ = client.Kill(ts.URL, row.ID, true) })
		waitState(t, ts.URL, row.ID, proto.StateRunning)
	}
	for _, row := range rows {
		if err := client.Kill(ts.URL, row.ID, true); err != nil {
			t.Fatal(err)
		}
		result := waitTerminal(t, ts.URL, row.ID)
		if !result.Result.CleanupOK {
			t.Fatal(result)
		}
	}
	entries, err := d.namedCaches.Inventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Protected() {
			t.Fatal(entry)
		}
	}
}

func TestCacheTreeSettlementRetryKeepsEarlierPublication(t *testing.T) {
	d, _ := testDaemon(t)
	j := newJob(proto.NewULID(), t.TempDir())
	j.Spec.CacheProjectID = proto.NewULID()
	j.Spec.Selection.Caches = []proto.CacheBinding{{Name: "first", Path: "one"}, {Name: "second", Path: "two"}}
	if err := os.MkdirAll(j.workspacePath(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := j.prepareNamedCacheTrees(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(j.workspacePath(), "one/value"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(j.workspacePath(), "two")
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.publishNamedCacheTrees(j, j.treeBaselines); err == nil {
		t.Fatal("expected invalid second tree")
	}
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(second, 0700); err != nil {
		t.Fatal(err)
	}
	if err := d.publishNamedCacheTrees(j, j.treeBaselines); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	key := namedcache.Key{Owner: d.cacheOwner(j), Project: j.Spec.CacheProjectID, Name: "first"}
	if _, err := d.namedCaches.RestoreTree(t.Context(), key, j.ID, workspace, "one"); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(workspace, "one/value")); err != nil || string(raw) != "keep" {
		t.Fatalf("retry erased successful publication: %q %v", raw, err)
	}
}
