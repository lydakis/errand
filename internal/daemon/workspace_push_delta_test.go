package daemon

import (
	"bytes"
	"context"
	"encoding/json"
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
