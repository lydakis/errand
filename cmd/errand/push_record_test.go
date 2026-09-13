package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

// A staged delta must be recoverable using just its delta and complete source
// identity, without serializing the unchanged inventory in every durable write.
func TestPushDeltaRecordOmitsInventoryAndResumes(t *testing.T) {
	root, destination, peer, ws := watchFixture(t)
	if err := os.WriteFile(filepath.Join(root, "value"), []byte("edited\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := client.PushOptions{PeerURL: peer, Workspace: ws.Name, Root: root}
	staged, err := client.PushChanges(opts)
	if err != nil {
		t.Fatal(err)
	}
	var record struct{ Request proto.PushRequest }
	found := false
	err = filepath.WalkDir(os.Getenv("XDG_STATE_HOME"), func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.Name() != "push.json" {
			return err
		}
		found = true
		raw, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &record)
	})
	if err != nil || !found || record.Request.Delta == nil || record.Request.SourceRoot == "" {
		t.Fatalf("missing recoverable delta record: found=%v err=%v", found, err)
	}
	if len(record.Request.Manifest.Entries) != 0 {
		t.Fatalf("delta recovery record redundantly contains %d full-inventory entries", len(record.Request.Manifest.Entries))
	}
	opts.Apply = true
	applied, err := client.PushChanges(opts)
	if err != nil || applied.ID != staged.ID {
		t.Fatalf("staged delta was not resumed: %s -> %s: %v", staged.ID, applied.ID, err)
	}
	if body, err := os.ReadFile(filepath.Join(destination, "value")); err != nil || string(body) != "edited\n" {
		t.Fatalf("resumed contents=%q err=%v", body, err)
	}
}
