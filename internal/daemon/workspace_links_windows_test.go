//go:build windows

package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/client"
)

func TestWorkspaceDirectorySymlinkPushOnWindows(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"target/value": "body"})
	// The lexical first link points through a link created later on extraction.
	for _, link := range []struct{ name, target string }{{"z", "target"}, {"a", "z"}} {
		if err := os.Symlink(link.target, filepath.Join(root, link.name)); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "links")
	if err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(d.workspaces.dir, ws.ID, "data")
	if body, err := os.ReadFile(filepath.Join(remote, "a", "value")); err != nil || string(body) != "body" {
		t.Fatalf("initial directory chain = %q, %v", body, err)
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../target", filepath.Join(root, "nested", "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PushChanges(client.PushOptions{PeerURL: ts.URL, Workspace: ws.Name, Root: root, Apply: true}); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(remote, "nested", "alias", "value")); err != nil || string(body) != "body" {
		t.Fatalf("pushed directory link = %q, %v", body, err)
	}
}
