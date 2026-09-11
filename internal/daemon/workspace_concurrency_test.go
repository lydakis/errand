package daemon

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func submitWorkspaceCommand(t *testing.T, url, root string, ws proto.Workspace, command string) string {
	t.Helper()
	id := proto.NewULID()
	spec := proto.Spec{WorkspaceID: ws.ID, CacheProjectID: ws.CacheProjectID, Argv: []string{"/bin/sh", "-c", command}, ManifestRoot: ws.Manifest.RootHash(), Selection: ws.Selection, Limits: proto.DefaultLimits()}
	resp := rawSubmitSpec(t, url, id, root, spec, ws.Manifest)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("workspace submission: %s %s", resp.Status, raw)
	}
	t.Cleanup(func() { _ = client.Kill(url, id, true) })
	return id
}

func TestConcurrentWorkspaceJobsKeepSiblingAndCachesAlive(t *testing.T) {
	for _, cached := range []bool{false, true} {
		name := "files"
		if cached {
			name = "cache"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			d, ts := concurrencyDaemon(t, 2, 1)
			root := workspaceWith(t, nil)
			opts := client.RunOptions{PeerURL: ts.URL, Root: root}
			if cached {
				opts.Caches = []proto.CacheBinding{{Name: "compiler", Path: "target"}}
			}
			ws, err := client.CreateWorkspace(opts, "experiment")
			if err != nil {
				t.Fatal(err)
			}
			first := submitWorkspaceCommand(t, ts.URL, root, ws, "sleep 60")
			waitState(t, ts.URL, first, proto.StateRunning)
			second := submitWorkspaceCommand(t, ts.URL, root, ws, "sleep 60")
			waitState(t, ts.URL, second, proto.StateRunning)
			rows, err := client.ListWorkspaces(ts.URL)
			if err != nil || len(rows) != 1 || !slices.Equal(rows[0].JobIDs, []string{first, second}) {
				t.Fatalf("workspace members: %+v %v", rows, err)
			}
			stats, err := client.StorageStatsDetailed(ts.URL)
			if err != nil || stats.Details == nil || len(stats.Details.Workspaces) != 1 || !slices.Equal(stats.Details.Workspaces[0].JobIDs, []string{first, second}) {
				t.Fatalf("workspace storage members: %+v %v", stats, err)
			}
			if cached && (len(stats.Details.NamedCaches) != 1 || stats.Details.NamedCaches[0].WorkspaceID != ws.ID || !slices.Equal(stats.Details.NamedCaches[0].JobIDs, []string{first, second})) {
				t.Fatalf("cache members: %+v", stats.Details.NamedCaches)
			}
			if err := client.Kill(ts.URL, first, true); err != nil {
				t.Fatal(err)
			}
			if st := waitTerminal(t, ts.URL, first); st.Result == nil || !st.Result.CleanupOK {
				t.Fatalf("first cleanup: %+v", st)
			}
			waitState(t, ts.URL, second, proto.StateRunning)
			if err := client.RemoveWorkspace(ts.URL, ws.Name); err == nil {
				t.Fatal("removed workspace while sibling running")
			}
			if cached {
				path := filepath.Join(d.workspaces.dir, ws.ID, "data", "target")
				if info, err := os.Lstat(path); err != nil || !info.IsDir() {
					t.Fatalf("cache unbound while sibling running: %v", err)
				}
				inventory, err := d.namedCaches.Inventory(t.Context())
				if err != nil || len(inventory) != 1 || !slices.Equal(inventory[0].Holders, []string{second}) {
					t.Fatalf("cache released early: %+v %v", inventory, err)
				}
			}
			// Natural exit must be as isolated as an explicit kill.
			third := submitWorkspaceCommand(t, ts.URL, root, ws, "echo finished > result")
			if st := waitTerminal(t, ts.URL, third); st.Result == nil || !st.Result.CleanupOK || st.Result.ExitCode == nil || *st.Result.ExitCode != 0 {
				t.Fatalf("third cleanup: %+v", st)
			}
			waitState(t, ts.URL, second, proto.StateRunning)
			if err := client.Kill(ts.URL, second, true); err != nil {
				t.Fatal(err)
			}
			waitTerminal(t, ts.URL, second)
			if cached {
				inventory, err := d.namedCaches.Inventory(t.Context())
				if err != nil || len(inventory) != 1 || inventory[0].Protected() {
					t.Fatalf("last job left cache leased: %+v %v", inventory, err)
				}
			}
			if err := client.RemoveWorkspace(ts.URL, ws.Name); err != nil {
				t.Fatal(err)
			}
		})
	}
}
