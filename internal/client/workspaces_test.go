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
