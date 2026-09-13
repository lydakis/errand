package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func TestWorkspaceDescriptorOmitsOnlyManifestAndPreservesOwnerBoundary(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"value": "initial\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root, Project: "descriptor", Artifacts: []string{"output"}}, "descriptor")
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(ts.URL + "/v0/workspaces/" + ws.Name + "?manifest=omit")
	if err != nil {
		t.Fatal(err)
	}
	var compact proto.Workspace
	err = json.NewDecoder(response.Body).Decode(&compact)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("descriptor status=%d err=%v", response.StatusCode, err)
	}
	want := ws
	want.Manifest = proto.Manifest{}
	if !reflect.DeepEqual(compact, want) {
		t.Fatalf("descriptor changed fields or retained manifest: got=%+v want=%+v", compact, want)
	}
	full, err := client.GetWorkspace(ts.URL, ws.Name)
	if err != nil || !reflect.DeepEqual(full, ws) {
		t.Fatalf("full GET changed after compact read: got=%+v err=%v", full, err)
	}
	record, err := d.workspaces.read(ws.ID)
	if err != nil || !reflect.DeepEqual(record.Manifest, ws.Manifest) {
		t.Fatalf("stored manifest changed: %v", err)
	}
	owner := Identity{Local: true, LocalUID: 1001}
	record.Owner = owner.Owner()
	if err := d.workspaces.write(record); err != nil {
		t.Fatal(err)
	}
	d.cfg.InsecureNoAuth = false
	for _, identity := range []Identity{owner, {Local: true, LocalUID: 1002}} {
		request := httptest.NewRequest(http.MethodGet, "/v0/workspaces/"+ws.ID+"?manifest=omit", nil)
		request.SetPathValue("id", ws.ID)
		reply := httptest.NewRecorder()
		d.handleWorkspaceGet(reply, request, identity)
		wantStatus := http.StatusNotFound
		if identity.LocalUID == owner.LocalUID {
			wantStatus = http.StatusOK
		}
		if reply.Code != wantStatus {
			t.Fatalf("owner=%d status=%d want=%d", identity.LocalUID, reply.Code, wantStatus)
		}
	}
}
