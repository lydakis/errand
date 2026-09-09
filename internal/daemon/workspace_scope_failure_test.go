package daemon

import (
	"errors"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func TestWorkspaceScopeWriteFailureReleasesVerifiedRuntime(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root, Caches: []proto.CacheBinding{{Name: "compiler", Path: "target"}}}, "experiment")
	if err != nil {
		t.Fatal(err)
	}
	d.writeProcessScope = func(string, any) error { return errors.New("injected scope write failure") }
	id := submitWorkspaceCommand(t, ts.URL, root, ws, "sleep 60")
	st := waitTerminal(t, ts.URL, id)
	if st.Result == nil || !st.Result.CleanupOK || !strings.Contains(st.Result.TransactionError, "injected scope write failure") {
		t.Fatalf("scope write failure should retain its error but finish cleanup: %+v", st)
	}
	row, err := client.GetWorkspace(ts.URL, ws.Name)
	if err != nil || len(row.JobIDs) != 0 {
		t.Fatalf("stuck workspace membership: %+v %v", row, err)
	}
	entries, err := d.namedCaches.Inventory(t.Context())
	if err != nil || len(entries) != 1 || entries[0].LeaseID != "" {
		t.Fatalf("cache remained leased: %+v %v", entries, err)
	}
	if err := client.RemoveWorkspace(ts.URL, ws.Name); err != nil {
		t.Fatalf("cannot remove cleaned workspace: %v", err)
	}
}
