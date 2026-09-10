package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDisabledAndPreviewHaveNoSideEffects(t *testing.T) {
	for _, preview := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "preview"}[preview], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing", "installation-id")
			var output bytes.Buffer
			r := New(Options{Version: "0.2.1", IDPath: path, Preview: preview, Output: &output})
			r.Admitted(Run{Transport: "ssh", Workspace: true})
			r.Finish("run", 0)
			if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
				t.Fatalf("created state: %v", err)
			}
			if preview {
				if !strings.Contains(output.String(), "job_admitted") || !strings.Contains(output.String(), "preview") {
					t.Fatal(output.String())
				}
			} else if output.Len() != 0 {
				t.Fatal(output.String())
			}
		})
	}
}

func TestDefaultOnShowsNoticeBeforeIdentityOrDelivery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id")
	var output bytes.Buffer
	opts := Options{Enabled: true, NoticeRequired: true, Version: "0.2.1", Token: "test-token", IDPath: path, Output: &output}
	if r := newReporter(opts, "http://unused.invalid"); r != nil {
		t.Fatal("first invocation enabled delivery")
	}
	if !strings.Contains(output.String(), "starting next time") || !strings.Contains(output.String(), "Details and opt-out: https://github.com/lydakis/errand/blob/main/docs/TELEMETRY.md") {
		t.Fatal(output.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("created identity before notice: %v", err)
	}
	if !noticeShown(path+".notice", &output) {
		t.Fatal("notice not remembered")
	}
	output.Reset()
	if !noticeShown(path+".notice", &output) || output.Len() != 0 {
		t.Fatal("repeated notice")
	}
	opts.IDPath = filepath.Join(t.TempDir(), "blocked", "id")
	if err := os.WriteFile(filepath.Dir(opts.IDPath), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if r := newReporter(opts, "http://unused.invalid"); r != nil {
			t.Fatal("enabled after failed notice persistence")
		}
	}
	if output.Len() != 0 {
		t.Fatal("printed notice without writable state")
	}
}

type noticeWriter func([]byte) (int, error)

func (w noticeWriter) Write(p []byte) (int, error) { return w(p) }

func TestNoticeReservationDoesNotAuthorizeSending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notice")
	writes := 0
	output := noticeWriter(func(p []byte) (int, error) {
		writes++
		var concurrent bytes.Buffer
		if noticeShown(path, &concurrent) || concurrent.Len() != 0 {
			t.Fatal("an invocation during notice display sent or printed again")
		}
		return 0, errors.New("stderr unavailable")
	})
	if noticeShown(path, output) || writes != 1 {
		t.Fatal("failed notice enabled sending")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("failed notice left a marker")
	}
	var retry bytes.Buffer
	if noticeShown(path, &retry) || retry.String() != notice || !noticeShown(path, io.Discard) {
		t.Fatal("could not retry a failed notice")
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if noticeShown(path, io.Discard) {
		t.Fatal("incomplete marker enabled sending")
	}
}

func TestDeliveryAndStableIdentity(t *testing.T) {
	var mu sync.Mutex
	var payloads []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var p map[string]any
		if err := json.NewDecoder(req.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		mu.Lock()
		payloads = append(payloads, p)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "installation-id")
	opts := Options{Enabled: true, NoticeRequired: true, Version: "0.2.1", Token: "public-token", IDPath: path, Output: io.Discard}
	if r := newReporter(opts, server.URL); r != nil {
		t.Fatal("first invocation must only show notice")
	}
	for i := 0; i < 2; i++ {
		r := newReporter(opts, server.URL)
		r.Admitted(Run{Transport: "ssh", Workspace: true, Caches: true})
		r.Finish("run", 0)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 4 {
		t.Fatalf("got %d events", len(payloads))
	}
	id := payloads[0]["distinct_id"]
	for _, p := range payloads {
		if p["distinct_id"] != id || p["api_key"] != "public-token" {
			t.Fatal(p)
		}
		props := p["properties"].(map[string]any)
		if props["$process_person_profile"] != false || props["$geoip_disable"] != true {
			t.Fatal(props)
		}
		if _, nested := props["run"]; nested {
			t.Fatal("run properties must be directly filterable")
		}
		if p["event"] == "job_admitted" && (props["transport"] != "ssh" || props["caches"] != true || props["start_detached"] != false) {
			t.Fatal(props)
		}
		if p["event"] == "cli_finished" && props["transport"] != nil {
			t.Fatal("run properties on non-admission event")
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("identity permissions: %v %v", info, err)
	}
}

func TestPayloadRejectsUnrecognizedStrings(t *testing.T) {
	var output bytes.Buffer
	r := New(Options{Preview: true, Version: "private-branch-secret", Output: &output})
	r.Admitted(Run{Transport: "ssh://private-host/token"})
	r.Finish("private-command-secret", 42)
	if strings.Contains(output.String(), "secret") || strings.Contains(output.String(), "private-host") {
		t.Fatal(output.String())
	}
	if !strings.Contains(output.String(), "unknown") {
		t.Fatal(output.String())
	}
}

func TestFinishBoundsSlowDelivery(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload event
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if payload.Name == "cli_finished" {
			close(finished)
			return
		}
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	r := newReporter(Options{Enabled: true, Version: "0.2.1", Token: "token", IDPath: filepath.Join(t.TempDir(), "id")}, server.URL)
	r.Admitted(Run{})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("no delivery")
	}
	start := time.Now()
	r.Finish("run", 0)
	if time.Since(start) > time.Second {
		t.Fatal("telemetry delayed exit")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("slow admission prevented exit event delivery")
	}
}

func TestSenderDoesNotFollowRedirects(t *testing.T) {
	var targetCalled bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalled = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer source.Close()
	send(context.Background(), source.URL, []byte(`{}`))
	if targetCalled {
		t.Fatal("followed redirect")
	}
}

func TestConcurrentIdentityCreationAndInvalidState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id")
	ids := make(chan string, 8)
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Go(func() {
			id, err := installationID(path)
			if err != nil {
				t.Error(err)
			}
			ids <- id
		})
	}
	workers.Wait()
	close(ids)
	var first string
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("identities differ: %q %q", first, id)
		}
	}
	if first == "" {
		t.Fatal("no identity")
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := newReporter(Options{Enabled: true, Version: "0.2.1", Token: "token", IDPath: path}, "http://unused.invalid"); r != nil {
		t.Fatal("used corrupt state")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := installationID(link); err == nil {
		t.Fatal("followed identity symlink")
	}
}
