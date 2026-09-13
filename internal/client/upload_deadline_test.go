package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

func TestUploadsUseAdmissionDeadlineAndPreserveOrigins(t *testing.T) {
	for _, operation := range []string{"create", "submit"} {
		for _, outcome := range []string{"success", "rejected", "server-error", "timeout"} {
			t.Run(operation+"/"+outcome, func(t *testing.T) {
				state := t.TempDir()
				t.Setenv("XDG_STATE_HOME", state)
				root := t.TempDir()
				if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "value"), []byte("source\n"), 0600); err != nil {
					t.Fatal(err)
				}
				oldDirect, oldMaintenance := directHTTP, maintenanceHTTP
				control, bulk := directTransport.Clone(), maintenanceTransport.Clone()
				control.ResponseHeaderTimeout = 20 * time.Millisecond
				bulk.ResponseHeaderTimeout = time.Second
				if outcome == "timeout" {
					bulk.ResponseHeaderTimeout = 40 * time.Millisecond
				}
				directCopy, bulkCopy := *oldDirect, *oldMaintenance
				directCopy.Transport, bulkCopy.Transport = control, bulk
				directHTTP, maintenanceHTTP = &directCopy, &bulkCopy
				t.Cleanup(func() {
					directHTTP, maintenanceHTTP = oldDirect, oldMaintenance
					control.CloseIdleConnections()
					bulk.CloseIdleConnections()
				})
				var mu sync.Mutex
				var paths []string
				canceled := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/diff") {
						http.NotFound(w, r)
						return
					}
					if _, err := io.Copy(io.Discard, r.Body); err != nil {
						t.Errorf("upload body: %v", err)
						return
					}
					mu.Lock()
					paths = append(paths, r.URL.Path)
					mu.Unlock()
					// This is post-upload durable admission work, beyond the control budget.
					select {
					case <-time.After(100 * time.Millisecond):
					case <-r.Context().Done():
						mu.Lock()
						canceled++
						mu.Unlock()
						return
					}
					switch outcome {
					case "rejected":
						http.Error(w, "definitely rejected", http.StatusBadRequest)
					case "server-error":
						http.Error(w, "publication unknown", http.StatusBadGateway)
					default:
						w.WriteHeader(http.StatusCreated)
						if operation == "create" {
							json.NewEncoder(w).Encode(proto.Workspace{ID: strings.TrimPrefix(r.URL.Path, "/v0/workspaces/"), Name: "slow"})
						} else {
							json.NewEncoder(w).Encode(proto.JobStatus{ID: strings.TrimPrefix(r.URL.Path, "/v0/jobs/"), State: proto.StateQueued})
						}
					}
				}))
				defer server.Close()
				opts := RunOptions{PeerURL: server.URL, Root: root, Argv: []string{"/bin/true"}, Detach: true, Stdout: io.Discard, Stderr: io.Discard}
				var succeeded bool
				var retained int
				if operation == "create" {
					_, err := CreateWorkspace(opts, "slow")
					succeeded = err == nil
					stats, err := workspaceTransferStats(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					if stats.Bytes > 0 {
						retained = 1
					}
				} else {
					code := runWithDetachNotifications(opts, make(chan os.Signal), testInterruptNotifications(), nil)
					succeeded = code == 0
					entries, err := os.ReadDir(filepath.Join(state, "errand", "jobs"))
					if err != nil {
						t.Fatal(err)
					}
					retained = len(entries)
				}
				server.Close() // Join canceled handlers before inspecting their observations.
				if succeeded != (outcome == "success") {
					t.Fatalf("success=%v outcome=%s", succeeded, outcome)
				}
				wantRetained := 1
				if outcome == "rejected" {
					wantRetained = 0
				}
				if retained != wantRetained {
					t.Fatalf("retained origins=%d want=%d", retained, wantRetained)
				}
				mu.Lock()
				defer mu.Unlock()
				wantAttempts := 1
				if operation == "submit" && outcome == "timeout" {
					wantAttempts = 3
				}
				if len(paths) != wantAttempts {
					t.Fatalf("upload attempts=%v want=%d", paths, wantAttempts)
				}
				for _, p := range paths {
					if p != paths[0] {
						t.Fatalf("uncertain upload changed identity: %v", paths)
					}
				}
				if outcome == "timeout" && canceled != wantAttempts {
					t.Fatalf("canceled requests=%d want=%d", canceled, wantAttempts)
				}
			})
		}
	}
}
