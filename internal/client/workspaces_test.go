package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// A workspace whose snapshot cannot be prepared rents no machine.
func TestCreateWorkspaceResolvesAfterSnapshot(t *testing.T) {
	resolved := false
	opts := RunOptions{Where: "gpu", Root: filepath.Join(t.TempDir(), "missing"), Resolve: func() ([]RunTarget, func(), error) {
		resolved = true
		return nil, nil, nil
	}}
	if _, err := CreateWorkspace(opts, "train"); err == nil || resolved {
		t.Fatalf("err %v, resolved %v", err, resolved)
	}
}
