package client

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestWorkspaceCreationChecksSelectionBeforeNegotiation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "private"), []byte("excluded after hashing"), 0600); err != nil {
		t.Fatal(err)
	}
	prep := prepareSnapshot(root, false, false)
	if prep.err != nil {
		t.Fatal(prep.err)
	}
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), []byte("private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	defer server.Close()
	_, err := createPreparedWorkspace(RunOptions{Root: root, PeerURL: server.URL}, prep, proto.Workspace{ID: proto.NewULID(), Name: "guard"})
	if err == nil {
		t.Fatal("accepted changed selection")
	}
	if requests.Load() != 0 {
		t.Fatal("sent source fingerprints before rejecting changed selection")
	}
}
