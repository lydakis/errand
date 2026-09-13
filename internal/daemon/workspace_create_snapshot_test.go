package daemon

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/client"
)

func TestWorkspaceCreationSnapshotReuseAndFallback(t *testing.T) {
	for _, scenario := range []string{"warm", "old-runner", "disabled", "evicted", "downgraded", "quota", "server-error"} {
		t.Run(scenario, func(t *testing.T) {
			d, err := New(Config{StateDir: t.TempDir(), InsecureNoAuth: true, CacheDisabled: scenario == "disabled"})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			handler := d.Handler()
			var uploads []int
			var uploadPaths []string
			var recording bool
			var invalidate func()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/snapshot/diff") && scenario == "old-runner" {
					http.NotFound(w, r)
					return
				}
				create := r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v0/workspaces/") && !strings.HasSuffix(r.URL.Path, "/diff")
				if recording && create {
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					r.Body.Close()
					r.Body = io.NopCloser(bytes.NewReader(raw))
					uploads = append(uploads, len(raw))
					uploadPaths = append(uploadPaths, r.URL.Path)
					if invalidate != nil {
						invalidate()
						invalidate = nil
					}
					if scenario == "server-error" {
						httpErrorCode(w, http.StatusInternalServerError, "snapshot_cache_miss", "server failure is not a retry authorization")
						return
					}
					if scenario == "downgraded" && strings.HasSuffix(r.URL.Path, "/snapshot") {
						http.NotFound(w, r)
						return
					}
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			body := strings.Repeat("x", 1<<20)
			root := workspaceWith(t, map[string]string{"bulk": body})
			first, err := client.CreateWorkspace(client.RunOptions{PeerURL: server.URL, Root: root}, "first")
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "evicted" {
				invalidate = func() {
					for _, e := range first.Manifest.Entries {
						if e.Path == "bulk" {
							d.cache.remove(e.SHA256)
						}
					}
				}
			}
			if scenario == "quota" {
				d.cfg.MaxLimits.MaxWorkspaceBytes = 64 << 10
			}
			recording = true
			second, err := client.CreateWorkspace(client.RunOptions{PeerURL: server.URL, Root: root}, "second")
			if scenario == "downgraded" || scenario == "quota" || scenario == "server-error" {
				if err == nil || len(uploads) != 1 {
					t.Fatalf("downgrade retried or succeeded: uploads=%v err=%v", uploads, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(d.workspaces.dir, second.ID, "data", "bulk"))
			if err != nil || string(got) != body {
				t.Fatalf("wrong reconstructed source: bytes=%d err=%v", len(got), err)
			}
			switch scenario {
			case "warm":
				if len(uploads) != 1 || uploads[0] > 16<<10 {
					t.Fatalf("warm creation retransmitted bodies: %v", uploads)
				}
			case "old-runner", "disabled":
				if len(uploads) != 1 || uploads[0] < 1<<20 || strings.HasSuffix(uploadPaths[0], "/snapshot") {
					t.Fatalf("old runner received partial creation: %v %v", uploads, uploadPaths)
				}
			case "evicted":
				if len(uploads) != 2 || uploads[0] > 16<<10 || uploads[1] < 1<<20 {
					t.Fatalf("wrong fallback sequence: %v", uploads)
				}
				if strings.TrimSuffix(uploadPaths[0], "/snapshot") != uploadPaths[1] {
					t.Fatalf("fallback changed creation identity: %v", uploadPaths)
				}
			}
		})
	}
}
