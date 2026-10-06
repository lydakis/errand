package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

// A workspace upload in progress is work: /v0/info counts it, so a cloud
// peer does not take the runner for idle, and setup does not restart under
// it. Once the upload ends it no longer counts.
func TestInfoCountsWorkspaceUploadInProgress(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"value": "initial\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "busy")
	if err != nil {
		t.Fatal(err)
	}
	transfers := func() int {
		t.Helper()
		resp, err := http.Get(ts.URL + "/v0/info")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var info proto.Info
		if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
			t.Fatal(err)
		}
		return info.Transfers
	}
	if n := transfers(); n != 0 {
		t.Fatalf("idle runner reports %d transfers", n)
	}

	// An upload whose body has not arrived yet.
	body, upload := io.Pipe()
	mw := multipart.NewWriter(upload)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v0/workspaces/"+ws.ID+"/push", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	defer func() {
		upload.CloseWithError(errors.New("upload abandoned"))
		<-done
	}()
	for deadline := time.Now().Add(5 * time.Second); transfers() != 1; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("upload in progress not counted")
		}
	}
	w := httptest.NewRecorder()
	d.handleSetupQuiesce(w, nil, Identity{Local: true})
	if w.Code != http.StatusConflict {
		t.Fatalf("setup quiesce during an upload = %d, want 409", w.Code)
	}

	upload.CloseWithError(errors.New("upload abandoned"))
	<-done
	for deadline := time.Now().Add(5 * time.Second); transfers() != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("ended upload still counted")
		}
	}
}

// A workspace transfer does not start while setup holds the runner for a
// restart: setup took the hold once nothing was in progress, and the restart
// would cut the transfer off.
func TestSetupQuiesceRefusesWorkspaceTransfers(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"value": "initial\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "held")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	d.handleSetupQuiesce(w, nil, Identity{Local: true})
	if w.Code != http.StatusCreated {
		t.Fatalf("setup quiesce = %d", w.Code)
	}
	for _, path := range []string{"/v0/workspaces/" + ws.ID + "/push", "/v0/workspaces/" + ws.ID + "/push/" + proto.NewULID() + "/apply", "/v0/workspaces/" + proto.NewULID()} {
		resp, err := http.Post(ts.URL+path, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		var apiErr proto.APIError
		json.NewDecoder(resp.Body).Decode(&apiErr)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || apiErr.Error != "runner is being reconfigured; retry on another peer" {
			t.Fatalf("POST %s while setup holds the runner = %s %q", path, resp.Status, apiErr.Error)
		}
	}
	d.mu.Lock()
	transfers := d.transfers
	d.mu.Unlock()
	if transfers != 0 {
		t.Fatalf("%d transfers counted after refusals", transfers)
	}
}
