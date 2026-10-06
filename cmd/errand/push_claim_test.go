package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

// A push to a lease asks for it once, after every local step and read has
// succeeded and right before its first request that changes the
// workspace, so the lease is not released mid-upload and a push that fails
// before then does not renew it.
func TestPushClaimsRightBeforeChangingWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name             string
		baseFails, retry bool
		refuse           bool
		claims, changes  int32
		ok               bool
	}{
		{name: "negotiation fails", baseFails: true},
		{name: "claim refused", refuse: true, claims: 1},
		{name: "upload retried", retry: true, claims: 1, changes: 3, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var changes, claims atomic.Int32
			var rejected atomic.Bool
			root, _, peer, ws := watchFixtureHandler(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/push/base") && tc.baseFails {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					if r.Method != http.MethodGet && !strings.HasSuffix(r.URL.Path, "/push/base") {
						changes.Add(1)
					}
					if strings.HasSuffix(r.URL.Path, "/push") && tc.retry && !rejected.Swap(true) {
						w.WriteHeader(http.StatusConflict)
						json.NewEncoder(w).Encode(proto.APIError{Code: proto.ErrorCodePushCheckpointChanged, Error: "checkpoint changed"})
						return
					}
					next.ServeHTTP(w, r)
				})
			})
			changes.Store(0) // setting up the workspace
			if err := os.WriteFile(filepath.Join(root, "value"), []byte("edited\n"), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := client.PushChanges(client.PushOptions{PeerURL: peer, Workspace: ws.Name, Root: root, Apply: true, BeforeSubmit: func() error {
				claims.Add(1)
				if n := changes.Load(); n != 0 {
					t.Errorf("claimed after %d requests changed the workspace", n)
				}
				if tc.refuse {
					return errors.New("lease cannot be used now")
				}
				return nil
			}})
			if (err == nil) != tc.ok || claims.Load() != tc.claims || changes.Load() != tc.changes {
				t.Fatalf("err=%v claims=%d changes=%d, want ok=%v claims=%d changes=%d", err, claims.Load(), changes.Load(), tc.ok, tc.claims, tc.changes)
			}
		})
	}
}
