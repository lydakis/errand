package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func TestWorkspaceRejectsUnsupportedLeaseFields(t *testing.T) {
	d, ts := testDaemon(t)
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: workspaceWith(t, nil)}, "experiment")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(d.workspaces.dir, ws.ID, "workspace.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"job_id", "legacy_exclusive"} {
		var record map[string]any
		if err := json.Unmarshal(original, &record); err != nil {
			t.Fatal(err)
		}
		record[field] = proto.NewULID()
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := d.workspaces.read(ws.ID); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("accepted %s: %v", field, err)
		}
	}
}
