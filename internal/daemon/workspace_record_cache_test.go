package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lydakis/errand/internal/client"
)

func TestWorkspaceRecordCacheFollowsReplacementAndIsolatesCallers(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"a": "one\n", "b": "two\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "cached")
	if err != nil {
		t.Fatal(err)
	}
	store := d.workspaces
	first, err := store.read(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Mutating a returned record must not leak into later reads.
	first.Manifest.Entries[0].Path = "mutated"
	first.JobIDs = append(first.JobIDs, "mutated")
	second, err := store.read(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Manifest.Entries[0].Path == "mutated" || len(second.JobIDs) != 0 {
		t.Fatalf("cached record was mutated through a returned copy: %+v", second)
	}

	// A durable replacement is observed immediately.
	second.Name = "renamed"
	if err := store.write(second); err != nil {
		t.Fatal(err)
	}
	third, err := store.read(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if third.Name != "renamed" {
		t.Fatalf("read returned a stale cached record: %q", third.Name)
	}
	if !reflect.DeepEqual(third.Manifest, second.Manifest) {
		t.Fatal("replacement changed the manifest")
	}

	// An in-place overwrite changes the stamp and is decoded, not served from cache.
	if err := os.WriteFile(filepath.Join(store.dir, ws.ID, "workspace.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read(ws.ID); err == nil {
		t.Fatal("corrupt workspace record was served from cache")
	}
}
