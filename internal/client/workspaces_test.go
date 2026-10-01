package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestWorkspaceListingRejectsIgnoredServerFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]proto.JobListEntry{{ID: proto.NewULID(), WorkspaceID: proto.NewULID()}})
	}))
	defer server.Close()
	if _, err := ListWorkspace(server.URL, proto.NewULID(), false); err == nil || !strings.Contains(err.Error(), "did not honor") {
		t.Fatalf("accepted unrelated jobs: %v", err)
	}
}

func TestWorkspaceRemovalUsesMetadataAndReportedSuccess(t *testing.T) {
	workspace := proto.Workspace{ID: proto.NewULID(), Name: "dev"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if r.URL.Query().Get("manifest") != "omit" {
				t.Error("removal requested the creation manifest")
			}
			json.NewEncoder(w).Encode(workspace)
		case http.MethodDelete:
			if r.URL.Path != "/v0/workspaces/"+workspace.ID {
				t.Errorf("invalid removal path %q", r.URL.Path)
			}
			json.NewEncoder(w).Encode(proto.WorkspaceRemoval{ID: workspace.ID, Name: workspace.Name, FreedBytes: 4096})
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()
	got, err := RemoveWorkspace(server.URL, workspace.Name)
	want := proto.WorkspaceRemoval{ID: workspace.ID, Name: workspace.Name, FreedBytes: 4096}
	if err != nil || got != want {
		t.Fatalf("removal=%+v err=%v, want %+v", got, err, want)
	}
}

func TestWorkspaceRemovalRequiresJSONReceipt(t *testing.T) {
	workspace := proto.Workspace{ID: proto.NewULID(), Name: "dev"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(workspace)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if _, err := RemoveWorkspace(server.URL, workspace.Name); err == nil {
		t.Fatal("accepted removal without its required receipt")
	}
}
