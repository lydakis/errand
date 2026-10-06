//go:build unix

package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestWaitingWorkspacePushesHoldNoDataHandle(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"value": "initial\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "queued")
	if err != nil {
		t.Fatal(err)
	}
	body := func() (*bytes.Buffer, string, int) {
		delta, err := changeops.PrepareSourceDelta(t.Context(), ws.Manifest, ws.Manifest, proto.DefaultLimits().MaxChangeBytes)
		if err != nil {
			t.Fatal(err)
		}
		var payload bytes.Buffer
		mw := multipart.NewWriter(&payload)
		part, err := mw.CreateFormField("metadata")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(part).Encode(proto.PushRequest{ID: proto.NewULID(), ClientID: "0123456789abcdef0123456789abcdef", Delta: &delta, SourceRoot: ws.Manifest.RootHash()}); err != nil {
			t.Fatal(err)
		}
		part, err = mw.CreateFormFile("workspace", "workspace.tar")
		if err != nil {
			t.Fatal(err)
		}
		split := payload.Len()
		if err := snapshot.PackPartial(part, root, delta.RemoteManifest, nil); err != nil {
			t.Fatal(err)
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		return &payload, mw.FormDataContentType(), split
	}
	push := func(r io.Reader, contentType string) (*httptest.ResponseRecorder, chan struct{}) {
		req := httptest.NewRequest("POST", "/", r)
		req.Header.Set("Content-Type", contentType)
		req.SetPathValue("id", ws.ID)
		response := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { defer close(done); d.handleWorkspacePush(response, req, Identity{}) }()
		return response, done
	}
	data := filepath.Join(d.workspaces.dir, ws.ID, "data")
	info, err := os.Stat(data)
	if err != nil {
		t.Fatal(err)
	}
	want := info.Sys().(*syscall.Stat_t)
	// Count descriptors by fstat: on macOS, stat of a /dev/fd entry does not
	// describe the directory it references.
	handles := func() int {
		entries, err := os.ReadDir("/dev/fd")
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range entries {
			fd, err := strconv.Atoi(e.Name())
			var st syscall.Stat_t
			if err == nil && syscall.Fstat(fd, &st) == nil && st.Dev == want.Dev && st.Ino == want.Ino {
				n++
			}
		}
		return n
	}

	payload, contentType, split := body()
	active := &pausedWorkspaceUpload{prefix: bytes.NewReader(payload.Bytes()[:split]), rest: bytes.NewReader(payload.Bytes()[split:]), stalled: make(chan struct{}), resume: make(chan struct{})}
	var resume sync.Once
	release := func() { resume.Do(func() { close(active.resume) }) }
	activeResponse, activeDone := push(active, contentType)
	var dones []chan struct{}
	// Every exit, including a failed assertion, finishes the pushes before
	// the test's directories are removed.
	defer func() {
		release()
		<-activeDone
		for _, done := range dones {
			<-done
		}
	}()
	<-active.stalled
	pinned := handles()
	if pinned != 1 {
		t.Fatalf("admitted push holds %d data directory handles, want 1", pinned)
	}

	const queued = 4
	waiting := make(chan string, queued)
	d.workspaces.mu.Lock()
	d.workspaces.testHookUploadWaiting = func(id string) {
		select {
		case waiting <- id:
		default:
		}
	}
	d.workspaces.mu.Unlock()
	var responses []*httptest.ResponseRecorder
	for range queued {
		payload, contentType, _ := body()
		response, done := push(payload, contentType)
		responses = append(responses, response)
		dones = append(dones, done)
	}
	for range queued {
		<-waiting
	}
	if got := handles(); got != pinned {
		t.Fatalf("%d waiting pushes hold %d data directory handles", queued, got-pinned)
	}

	release()
	<-activeDone
	for _, done := range dones {
		<-done
	}
	for i, response := range append([]*httptest.ResponseRecorder{activeResponse}, responses...) {
		if response.Code != http.StatusCreated {
			t.Fatalf("push %d: %d %s", i, response.Code, response.Body.String())
		}
	}
	if got := handles(); got != 0 {
		t.Fatalf("finished pushes left %d data directory handles open", got)
	}
}
