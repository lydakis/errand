package daemon

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestPushReusesSnapshotBodies(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"edit": "before\n", "unchanged": strings.Repeat("x", 1<<20)})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "small-push")
	if err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(d.workspaces.dir, ws.ID, "data")
	// Cached source content must never be taken from the live, writable tree.
	if err := os.WriteFile(filepath.Join(remote, "unchanged"), []byte("runner edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "edit"), []byte("after!\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stats client.TransferStats
	opts := client.PushOptions{PeerURL: ts.URL, Workspace: ws.Name, Root: root, Path: "edit", Apply: true, Stats: &stats}
	if _, err := client.PushChanges(opts); err != nil {
		t.Fatal(err)
	}
	if stats.ChangedPaths != 1 || stats.TransferredBytes > 16<<10 {
		t.Fatalf("one-line push retransmitted unchanged content: %+v", stats)
	}
	if body, err := os.ReadFile(filepath.Join(remote, "edit")); err != nil || string(body) != "after!\n" {
		t.Fatalf("edit not applied: %q %v", body, err)
	}
	opts.Path = ""
	if _, err := client.PushChanges(opts); err != nil {
		t.Fatal(err)
	}
	if stats.ChangedPaths != 0 || stats.TransferredBytes > 16<<10 {
		t.Fatalf("no-op push retransmitted content: %+v", stats)
	}
	if body, err := os.ReadFile(filepath.Join(remote, "unchanged")); err != nil || string(body) != "runner edit\n" {
		t.Fatalf("push overwrote unrelated runner edits: %q %v", body, err)
	}
}

func TestSnapshotIngestionContinuesAfterSourceFailure(t *testing.T) {
	d, _ := testDaemon(t)
	root := workspaceWith(t, map[string]string{"a": "unavailable", "b": "cache me"})
	manifest, err := snapshot.Build(root, []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	if err := d.cacheSnapshotSource(context.Background(), root, manifest, nil); err == nil {
		t.Fatal("missing source was not reported")
	}
	for _, e := range manifest.Entries {
		if e.Path == "b" && len(d.cache.Missing([]proto.BlobRef{{SHA256: e.SHA256, Size: e.Size}})) != 0 {
			t.Fatal("one unavailable source prevented caching a later file")
		}
	}
}

func TestPushSnapshotFallback(t *testing.T) {
	for _, failure := range []string{"old-runner", "disabled", "cold", "evicted", "corrupt", "corrupt-unremovable"} {
		t.Run(failure, func(t *testing.T) {
			if failure == "corrupt-unremovable" && os.Geteuid() == 0 {
				t.Skip("root bypasses directory permissions")
			}
			d, err := New(Config{StateDir: t.TempDir(), InsecureNoAuth: true, CacheDisabled: failure == "disabled"})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			var bodies []int
			var invalidate func()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/push/diff") && failure == "old-runner" {
					http.NotFound(w, r)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/push") {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					bodies = append(bodies, len(body))
					r.Body.Close()
					r.Body = io.NopCloser(bytes.NewReader(body))
					if invalidate != nil {
						invalidate()
						invalidate = nil
					}
				}
				d.Handler().ServeHTTP(w, r)
			}))
			defer server.Close()
			root := workspaceWith(t, map[string]string{"edit": "before\n", "unchanged": strings.Repeat("x", 1<<20)})
			ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: server.URL, Root: root}, "fallback")
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range ws.Manifest.Entries {
				if e.Path != "unchanged" || d.cache == nil {
					continue
				}
				invalidate = func() {
					var err error
					if strings.HasPrefix(failure, "corrupt") {
						err = os.WriteFile(d.cache.path(e.SHA256), bytes.Repeat([]byte("z"), int(e.Size)), 0600)
						if err == nil && failure == "corrupt-unremovable" {
							dir := filepath.Dir(d.cache.path(e.SHA256))
							err = os.Chmod(dir, 0500)
							t.Cleanup(func() { os.Chmod(dir, 0700) })
						}
					} else {
						err = os.Remove(d.cache.path(e.SHA256))
					}
					if err != nil {
						t.Error(err)
					}
				}
			}
			if failure == "cold" {
				invalidate()
				invalidate = nil
			} else if failure != "evicted" && !strings.HasPrefix(failure, "corrupt") {
				invalidate = nil
			}
			if err := os.WriteFile(filepath.Join(root, "edit"), []byte("after!\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var stats client.TransferStats
			if _, err := client.PushChanges(client.PushOptions{PeerURL: server.URL, Workspace: ws.Name, Root: root, Apply: true, Stats: &stats}); err != nil {
				t.Fatal(err)
			}
			wantUploads := 1
			if failure == "evicted" || strings.HasPrefix(failure, "corrupt") {
				wantUploads = 2
				if len(bodies) > 0 && bodies[0] > 16<<10 {
					t.Fatal("first upload did not use negotiation")
				}
			}
			var total int64
			for _, size := range bodies {
				total += int64(size)
			}
			if len(bodies) != wantUploads || total != stats.TransferredBytes || total < 1<<20 {
				t.Fatalf("fallback uploads=%v stats=%+v", bodies, stats)
			}
			if body, err := os.ReadFile(filepath.Join(d.workspaces.dir, ws.ID, "data", "edit")); err != nil || string(body) != "after!\n" {
				t.Fatalf("fallback did not apply: %q %v", body, err)
			}
		})
	}
}
