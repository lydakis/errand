package daemon

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func TestWorkspaceRecoveryIsolatesUnreadableLeaseState(t *testing.T) {
	for _, damaged := range []string{"workspace", "reference", "missing-metadata"} {
		t.Run(damaged, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			d, ts := testDaemon(t)
			root := workspaceWith(t, map[string]string{"value": "keep"})
			ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "damaged")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "healthy"); err != nil {
				t.Fatal(err)
			}
			j := newJob(proto.NewULID(), "")
			j.Dir = filepath.Join(d.jobsDir(), j.ID)
			j.Spec = proto.Spec{WorkspaceID: ws.ID, ManifestRoot: ws.Manifest.RootHash(), Selection: ws.Selection, Argv: []string{"true"}, Limits: proto.DefaultLimits()}
			if err := os.Mkdir(j.Dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := j.writeJSON("spec.json", proto.NewReceiptSpec(j.Spec)); err != nil {
				t.Fatal(err)
			}
			if err := d.acquireWorkspace(t.Context(), j); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(d.workspaces.dir, ws.ID, "workspace.json")
			if damaged == "reference" {
				file = filepath.Join(j.Dir, workspaceLeaseFile)
			}
			if err := os.WriteFile(file, []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
			if damaged == "missing-metadata" {
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			}
			ts.Close()
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := New(d.cfg)
			if err != nil {
				t.Fatalf("one damaged lease blocked startup: %v", err)
			}
			defer restarted.Close()
			server := httptest.NewServer(restarted.Handler())
			defer server.Close()
			if err := client.RemoveWorkspace(server.URL, "damaged"); err == nil {
				t.Fatal("removed workspace with uncertain lease")
			}
			value, err := os.ReadFile(filepath.Join(j.workspacePath(), "value"))
			if err != nil || string(value) != "keep" {
				t.Fatalf("protected files: %q %v", value, err)
			}
			var out bytes.Buffer
			if code := client.Run(client.RunOptions{PeerURL: server.URL, Root: root, Workspace: "healthy", Argv: []string{"true"}, Stdout: &out, Stderr: &out}); code != 0 {
				t.Fatalf("healthy workspace failed: %d %s", code, &out)
			}
		})
	}
}

func TestWorkspaceMetadataWaitDoesNotHoldAdmissionLock(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "experiment")
	if err != nil {
		t.Fatal(err)
	}
	id := proto.NewULID()
	spec := proto.Spec{WorkspaceID: ws.ID, ManifestRoot: ws.Manifest.RootHash(), Selection: ws.Selection, Argv: []string{"true"}, Limits: proto.DefaultLimits()}
	d.workspaces.mu.Lock()
	locked := true
	done := make(chan *http.Response, 1)
	go func() { done <- rawSubmitSpec(t, ts.URL, id, root, spec, ws.Manifest) }()
	defer func() {
		if locked {
			d.workspaces.mu.Unlock()
		}
		select {
		case resp := <-done:
			resp.Body.Close()
		case <-time.After(5 * time.Second):
			t.Error("submission failed to finish")
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	var admitted bool
	for time.Now().Before(deadline) {
		if d.mu.TryLock() {
			_, admitted = d.jobs[id]
			d.mu.Unlock()
			if admitted {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	if !admitted {
		t.Fatal("workspace metadata wait blocked admission control")
	}
	if _, err := client.List(ts.URL); err != nil {
		t.Fatalf("status/list blocked by workspace metadata: %v", err)
	}
	d.workspaces.mu.Unlock()
	locked = false
}
