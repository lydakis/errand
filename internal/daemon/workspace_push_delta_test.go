package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestPushDeltaValidatesRetainedBaseAndFullSourceLimit(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"bulk": strings.Repeat("b", 128<<10), "value": "initial\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "delta")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "value"), []byte("updated\n"), 0600); err != nil {
		t.Fatal(err)
	}
	current, err := snapshot.Build(root, []string{"bulk", "value"})
	if err != nil {
		t.Fatal(err)
	}
	delta, err := changes.PrepareSourceDelta(context.Background(), ws.Manifest, current, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"stale", "forged", "quota", "valid"} {
		t.Run(scenario, func(t *testing.T) {
			bundle := delta
			req := proto.PushRequest{ID: proto.NewULID(), ClientID: "0123456789abcdef0123456789abcdef", Delta: &bundle, SourceRoot: current.RootHash()}
			limit := d.cfg.MaxLimits.MaxWorkspaceBytes
			defer func() { d.cfg.MaxLimits.MaxWorkspaceBytes = limit }()
			switch scenario {
			case "stale":
				bundle.BaselineRoot = strings.Repeat("0", 64)
			case "forged":
				req.SourceRoot = strings.Repeat("0", 64)
			case "quota":
				d.cfg.MaxLimits.MaxWorkspaceBytes = 64 << 10
			}
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			metadata, err := mw.CreateFormField("metadata")
			if err != nil {
				t.Fatal(err)
			}
			if err := json.NewEncoder(metadata).Encode(req); err != nil {
				t.Fatal(err)
			}
			archive, err := mw.CreateFormFile("workspace", "workspace.tar")
			if err != nil {
				t.Fatal(err)
			}
			if err := snapshot.PackPartial(archive, root, bundle.RemoteManifest, nil); err != nil {
				t.Fatal(err)
			}
			if err := mw.Close(); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("POST", "/", &body)
			r.SetPathValue("id", ws.ID)
			r.Header.Set("Content-Type", mw.FormDataContentType())
			w := httptest.NewRecorder()
			d.handleWorkspacePush(w, r, Identity{})
			want := 409
			if scenario == "quota" {
				want = 400
			} else if scenario == "valid" {
				want = 201
			}
			if w.Code != want {
				t.Fatalf("status=%d want=%d: %s", w.Code, want, w.Body)
			}
		})
	}
}

// Pushes reuse the creation base retained with the workspace record, but only
// for the record bytes it came from, and still compare it with each checkpoint.
func TestPushChecksRetainedCreationBaseAgainstCurrentRecords(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"value": "initial\n", "other": "fixed\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "creation")
	if err != nil {
		t.Fatal(err)
	}
	opts := client.PushOptions{PeerURL: ts.URL, Root: root, Workspace: ws.Name, Apply: true}
	edit := 0
	push := func() error {
		edit++
		if err := os.WriteFile(filepath.Join(root, "value"), []byte(fmt.Sprintf("edit %d\n", edit)), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := client.PushChanges(opts)
		return err
	}
	for range 2 {
		if err := push(); err != nil {
			t.Fatal(err)
		}
	}
	store := d.workspaces
	original, err := store.read(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := store.read(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	changed.Manifest.Entries[len(changed.Manifest.Entries)-1].SHA256 = strings.Repeat("0", 64)
	if err := store.write(changed); err != nil {
		t.Fatal(err)
	}
	if err := push(); err == nil || !strings.Contains(err.Error(), "creation snapshot does not match") {
		t.Fatalf("push with a replaced creation snapshot: %v", err)
	}
	if err := store.write(original); err != nil {
		t.Fatal(err)
	}
	if err := push(); err != nil {
		t.Fatalf("restored creation snapshot: %v", err)
	}

	// A checkpoint naming another creation snapshot refuses the retained base.
	clients, err := os.ReadDir(filepath.Join(store.dir, ws.ID, "push"))
	if err != nil || len(clients) != 1 {
		t.Fatalf("push clients: %v, %v", clients, err)
	}
	path := filepath.Join(store.dir, ws.ID, "push", clients[0].Name(), "checkpoint.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	state["initial_root"] = strings.Repeat("0", 64)
	if raw, err = json.Marshal(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := push(); err == nil || !strings.Contains(err.Error(), "creation snapshot does not match") {
		t.Fatalf("push against a changed checkpoint: %v", err)
	}
}
