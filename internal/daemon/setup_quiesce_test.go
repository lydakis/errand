package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

func TestSetupQuiesceBlocksAdmissionUntilReleased(t *testing.T) {
	_, client := unixDaemon(t, Config{})
	res, err := client.Post("http://errand/v0/setup/quiesce", "application/json", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("quiesce = %s: %s", res.Status, body)
	}
	var lease proto.SetupQuiesce
	if err := json.NewDecoder(res.Body).Decode(&lease); err != nil {
		res.Body.Close()
		t.Fatal(err)
	}
	res.Body.Close()
	if lease.Token == "" || lease.ExpiresAt.IsZero() {
		t.Fatalf("lease = %+v", lease)
	}

	jobID := proto.NewULID()
	manifest := proto.Manifest{}
	spec := proto.Spec{
		Argv: []string{"/usr/bin/true"}, ManifestRoot: manifest.RootHash(),
		Limits: proto.DefaultLimits(), ChangeClientID: testChangeClientID,
	}
	submit := func() *http.Response {
		template := idempotentSubmitRequest(t, jobID, spec, manifest)
		body, err := io.ReadAll(template.Body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPut, "http://errand/v0/jobs/"+jobID, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", template.Header.Get("Content-Type"))
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	blocked := submit()
	blocked.Body.Close()
	if blocked.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("submission during setup = %s, want 503", blocked.Status)
	}

	releaseBody, _ := json.Marshal(proto.SetupQuiesceRelease{Token: lease.Token})
	release, err := http.NewRequest(http.MethodDelete, "http://errand/v0/setup/quiesce", bytes.NewReader(releaseBody))
	if err != nil {
		t.Fatal(err)
	}
	release.Header.Set("Content-Type", "application/json")
	released, err := client.Do(release)
	if err != nil {
		t.Fatal(err)
	}
	released.Body.Close()
	if released.StatusCode != http.StatusNoContent {
		t.Fatalf("release = %s", released.Status)
	}

	admitted := submit()
	admitted.Body.Close()
	if admitted.StatusCode != http.StatusCreated {
		t.Fatalf("submission after release = %s, want 201", admitted.Status)
	}
}

func TestSetupQuiesceMarksRunnerBusyUntilReleased(t *testing.T) {
	_, client := unixDaemon(t, Config{})
	res, err := client.Post("http://errand/v0/setup/quiesce", "application/json", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	var lease proto.SetupQuiesce
	if err := json.NewDecoder(res.Body).Decode(&lease); err != nil {
		res.Body.Close()
		t.Fatal(err)
	}
	res.Body.Close()

	infoRes, err := client.Get("http://errand/v0/info")
	if err != nil {
		t.Fatal(err)
	}
	var info proto.Info
	if err := json.NewDecoder(infoRes.Body).Decode(&info); err != nil {
		infoRes.Body.Close()
		t.Fatal(err)
	}
	infoRes.Body.Close()
	if !info.Busy {
		t.Fatal("quiesced runner was advertised as available")
	}

	releaseBody, _ := json.Marshal(proto.SetupQuiesceRelease{Token: lease.Token})
	release, err := http.NewRequest(http.MethodDelete, "http://errand/v0/setup/quiesce", bytes.NewReader(releaseBody))
	if err != nil {
		t.Fatal(err)
	}
	release.Header.Set("Content-Type", "application/json")
	released, err := client.Do(release)
	if err != nil {
		t.Fatal(err)
	}
	released.Body.Close()

	infoRes, err = client.Get("http://errand/v0/info")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(infoRes.Body).Decode(&info); err != nil {
		infoRes.Body.Close()
		t.Fatal(err)
	}
	infoRes.Body.Close()
	if info.Busy {
		t.Fatal("released idle runner remained busy")
	}
}

func TestSetupQuiesceRefusesAnActiveRunner(t *testing.T) {
	d, client := unixDaemon(t, Config{})
	j := newJob(proto.NewULID(), t.TempDir())
	j.state = proto.StateStaging
	d.mu.Lock()
	d.queue = append(d.queue, j)
	d.mu.Unlock()

	res, err := client.Post("http://errand/v0/setup/quiesce", "application/json", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("quiescing active runner = %s, want 409", res.Status)
	}
	var apiErr proto.APIError
	if err := json.NewDecoder(res.Body).Decode(&apiErr); err != nil {
		t.Fatal(err)
	}
	if apiErr.Error != "runner has active jobs (1 staging); wait until it is idle before restarting" {
		t.Fatalf("active runner diagnostic = %q", apiErr.Error)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.setupQuiesceToken != "" {
		t.Fatal("active runner was quiesced")
	}
}

func TestSetupQuiesceCanReplaceAnExpiredLease(t *testing.T) {
	d := &Daemon{
		jobs: map[string]*Job{}, running: map[string]*Job{},
		setupQuiesceToken: "expired", setupQuiesceUntil: time.Now().Add(-time.Second),
	}
	w := httptest.NewRecorder()
	d.handleSetupQuiesce(w, httptest.NewRequest(http.MethodPost, "/v0/setup/quiesce", nil), Identity{Local: true})
	if w.Code != http.StatusCreated {
		t.Fatalf("replacing expired lease = %d, want 201", w.Code)
	}
	if d.setupQuiesceToken == "" || d.setupQuiesceToken == "expired" {
		t.Fatalf("replacement lease token = %q", d.setupQuiesceToken)
	}
}

func TestSetupQuiesceIsLocalOnly(t *testing.T) {
	d, err := New(Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ts := httptest.NewServer(d.Handler())
	defer ts.Close()

	res, err := http.Post(ts.URL+"/v0/setup/quiesce", "application/json", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("tailnet setup quiesce = %s, want 403", res.Status)
	}
}
