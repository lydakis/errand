package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

type pausedWorkspaceUpload struct {
	prefix, rest    io.Reader
	stalled, resume chan struct{}
	once            sync.Once
}

func (p *pausedWorkspaceUpload) Read(b []byte) (int, error) {
	if p.prefix != nil {
		n, err := p.prefix.Read(b)
		if n > 0 {
			return n, nil
		}
		if err != io.EOF {
			return n, err
		}
		p.prefix = nil
	}
	p.once.Do(func() { close(p.stalled) })
	<-p.resume
	return p.rest.Read(b)
}

func TestWorkspaceUploadDoesNotBlockCommandsOrRetargetReplacement(t *testing.T) {
	for _, reuseID := range []bool{false, true} {
		t.Run(fmt.Sprintf("reuse-id=%v", reuseID), func(t *testing.T) {
			d, ts := testDaemon(t)
			root := workspaceWith(t, map[string]string{"value": "initial\n"})
			ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "upload")
			if err != nil {
				t.Fatal(err)
			}
			var payload bytes.Buffer
			mw := multipart.NewWriter(&payload)
			part, err := mw.CreateFormField("metadata")
			if err != nil {
				t.Fatal(err)
			}
			if err := json.NewEncoder(part).Encode(proto.PushRequest{ID: proto.NewULID(), ClientID: "0123456789abcdef0123456789abcdef", Manifest: ws.Manifest}); err != nil {
				t.Fatal(err)
			}
			part, err = mw.CreateFormFile("workspace", "workspace.tar")
			if err != nil {
				t.Fatal(err)
			}
			split := payload.Len()
			if err := snapshot.PackPartial(part, root, ws.Manifest, nil); err != nil {
				t.Fatal(err)
			}
			if err := mw.Close(); err != nil {
				t.Fatal(err)
			}
			p := &pausedWorkspaceUpload{prefix: bytes.NewReader(payload.Bytes()[:split]), rest: bytes.NewReader(payload.Bytes()[split:]), stalled: make(chan struct{}), resume: make(chan struct{})}
			var resume sync.Once
			release := func() { resume.Do(func() { close(p.resume) }) }
			defer release()
			req := httptest.NewRequest("POST", "/", p)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			req.SetPathValue("id", ws.ID)
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); d.handleWorkspacePush(response, req, Identity{}) }()
			<-p.stalled
			operation := make(chan error, 1)
			go func() {
				var out bytes.Buffer
				if code := client.Run(client.RunOptions{PeerURL: ts.URL, Root: root, Workspace: ws.Name, Argv: []string{"true"}, Stdout: &out, Stderr: &out}); code != 0 {
					operation <- io.ErrUnexpectedEOF
					return
				}
				operation <- client.RemoveWorkspace(ts.URL, ws.Name)
			}()
			select {
			case err := <-operation:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				release()
				<-done
				<-operation
				t.Fatal("upload blocked workspace command/removal")
			}
			replacement, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, ws.Name)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus := http.StatusNotFound
			if reuseID {
				// The HTTP creation API accepts caller-supplied IDs. Even reusing one
				// after deletion must not let an old upload address a different tree.
				row, err := d.workspaces.read(replacement.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(d.workspaces.dir, row.ID), filepath.Join(d.workspaces.dir, ws.ID)); err != nil {
					t.Fatal(err)
				}
				row.ID = ws.ID
				if err := d.workspaces.write(row); err != nil {
					t.Fatal(err)
				}
				replacement.ID = ws.ID
				wantStatus = http.StatusConflict
			}
			release()
			<-done
			if response.Code != wantStatus {
				t.Fatalf("removed target: %d %s", response.Code, response.Body.String())
			}
			if _, err := os.Stat(filepath.Join(d.workspaces.dir, replacement.ID, "push")); !os.IsNotExist(err) {
				t.Fatalf("upload retargeted replacement: %v", err)
			}
			entries, err := os.ReadDir(d.workspaces.dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if len(e.Name()) > 0 && e.Name()[0] == '.' {
					t.Fatalf("upload debris: %s", e.Name())
				}
			}
		})
	}
}

func TestWorkspacePushSeparatesSourceAndChangeLimits(t *testing.T) {
	d, ts := testDaemon(t)
	d.cfg.MaxLimits.MaxChangeBytes = 64 << 10
	root := workspaceWith(t, map[string]string{"bulk": string(bytes.Repeat([]byte("b"), 128<<10)), "value": "initial\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "limits")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "value"), []byte("updated\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := client.PushOptions{PeerURL: ts.URL, Root: root, Workspace: ws.Name, Apply: true}
	if _, err := client.PushChanges(opts); err != nil {
		t.Fatalf("small delta on larger workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "bulk"), bytes.Repeat([]byte("c"), 128<<10), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PushChanges(opts); err == nil {
		t.Fatal("change byte limit was bypassed")
	}
	got, err := os.ReadFile(filepath.Join(d.workspaces.dir, ws.ID, "data", "bulk"))
	if err != nil || !bytes.Equal(got, bytes.Repeat([]byte("b"), 128<<10)) {
		t.Fatal("oversized change mutated destination", err)
	}
}

func TestWorkspaceStoreRecoversUnpublishedUpload(t *testing.T) {
	dir := t.TempDir()
	abandoned := filepath.Join(dir, ".push-upload-interrupted")
	if err := os.MkdirAll(filepath.Join(abandoned, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abandoned, "nested", "partial"), []byte("partial upload"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := openWorkspaces(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.root.Close()
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatal("interrupted upload survived restart", err)
	}
}

func TestWorkspaceTransferGCReportsPartialFailure(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"value": "initial\n"})
	var workspaces []proto.Workspace
	for _, name := range []string{"bad", "good"} {
		ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.PushChanges(client.PushOptions{PeerURL: ts.URL, Root: root, Workspace: ws.Name}); err != nil {
			t.Fatal(err)
		}
		workspaces = append(workspaces, ws)
	}
	for i, ws := range workspaces {
		entries, err := os.ReadDir(filepath.Join(d.workspaces.dir, ws.ID, "push"))
		if err != nil {
			t.Fatal(err)
		}
		sessionDir := filepath.Join(d.workspaces.dir, ws.ID, "push", entries[0].Name())
		if i == 0 {
			if err := os.WriteFile(filepath.Join(sessionDir, "checkpoint.json"), []byte("{broken"), 0600); err != nil {
				t.Fatal(err)
			}
		} else {
			attempts, err := os.ReadDir(filepath.Join(sessionDir, "attempts"))
			if err != nil {
				t.Fatal(err)
			}
			for _, attempt := range attempts {
				file := filepath.Join(sessionDir, "attempts", attempt.Name(), "attempt.json")
				raw, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				var a changeops.TransferAttempt
				if err := json.Unmarshal(raw, &a); err != nil {
					t.Fatal(err)
				}
				a.CreatedAt = time.Now().Add(-time.Hour)
				raw, err = json.Marshal(a)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	result, err := client.RemoteTransferGC(ts.URL, time.Second, false)
	if err == nil || len(result.Failures) != 1 || result.Removed != 1 {
		t.Fatalf("partial GC: %+v %v", result, err)
	}
}
