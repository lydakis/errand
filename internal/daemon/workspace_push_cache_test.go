package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func TestPushPreservesWorkspaceCaches(t *testing.T) {
	d, server := testDaemon(t)
	root := workspaceWith(t, map[string]string{"value": "initial", "target/local": "local cache"})
	ws, err := client.CreateWorkspace(client.RunOptions{
		PeerURL: server.URL, Root: root,
		Caches: []proto.CacheBinding{{Name: "compiler", Path: "target"}},
	}, "cached")
	if err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(d.workspaces.dir, ws.ID, "data")
	if err := os.MkdirAll(filepath.Join(remote, "target"), 0700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(root, "value"):              "edited",
		filepath.Join(remote, "target", "remote"): "remote cache",
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	opts := client.PushOptions{PeerURL: server.URL, Root: root, Workspace: ws.Name, Apply: true}
	if _, err := client.PushChanges(opts); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"value": "edited", "target/remote": "remote cache"} {
		got, err := os.ReadFile(filepath.Join(remote, path))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(remote, "target", "local")); !os.IsNotExist(err) {
		t.Fatalf("uploaded local cache: %v", err)
	}
	// Restoring fixed cache bindings must not mask actual selection changes.
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), []byte("other/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PushChanges(opts); err == nil || !strings.Contains(err.Error(), "selection policy differs") {
		t.Fatalf("changed policy: %v", err)
	}
}
