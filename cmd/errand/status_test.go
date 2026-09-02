package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

func TestCmdStatusShowsOneJobsExecutionAndArtifacts(t *testing.T) {
	id := proto.NewULID()
	admitted := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	started := admitted.Add(time.Minute)
	finished := started.Add(2*time.Minute + 3*time.Second)
	exitCode := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/jobs/"+id {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "state": proto.StateExited, "admitted_at": admitted, "project": "atlas",
			"spec": map[string]any{"argv": []string{"nix", "build"}, "workdir": "vm"},
			"result": proto.Result{
				Started: true, StartedAt: &started, FinishedAt: &finished, DurationMS: 123000,
				ExitCode: &exitCode, ChangesOK: true, CleanupOK: true, LogsComplete: true,
				Changes: &proto.ChangeSummary{Paths: []string{"dist/app"}, PathCount: 1, Bytes: 42},
			},
		})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := cmdStatusTo([]string{"--url", server.URL, id}, &stdout, &stderr); code != 0 {
		t.Fatalf("status exit = %d; stderr=%q", code, stderr.String())
	}
	for _, want := range []string{
		server.URL + "/" + id, "exited", `"nix" "build"`, "atlas", "vm", "2m3s", "exit 0",
		"retained (complete)", "Workspace changes", "dist/app", "42 bytes",
		"errand attach " + server.URL + "/" + id, "errand fetch " + server.URL + "/" + id,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestCmdStatusJSONIncludesPeerAndJobDetails(t *testing.T) {
	id := proto.NewULID()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "state": proto.StateRunning,
			"spec": map[string]any{"argv": []string{"sleep", "10"}},
		})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := cmdStatusTo([]string{"--json", "--url", server.URL, id}, &stdout, &stderr); code != 0 {
		t.Fatalf("status --json exit = %d; stderr=%q", code, stderr.String())
	}
	var got struct {
		Peer   string            `json:"peer"`
		Handle string            `json:"handle"`
		ID     string            `json:"id"`
		State  string            `json:"state"`
		Spec   proto.ReceiptSpec `json:"spec"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decoding status JSON: %v; output=%q", err, stdout.String())
	}
	if got.Peer != server.URL || got.Handle != server.URL+"/"+id || got.ID != id ||
		got.State != proto.StateRunning || len(got.Spec.Argv) != 2 {
		t.Fatalf("status JSON = %+v", got)
	}
}

func TestCmdStatusExplainsEmptyWorkspaceAndIncompleteRetention(t *testing.T) {
	id := proto.NewULID()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(proto.JobDetails{
			JobStatus: proto.JobStatus{ID: id, State: proto.StateExited, Result: &proto.Result{
				ChangesOK: false, CleanupOK: true, LogsComplete: true,
				TransactionError: "collection failed",
			}},
			Spec: proto.ReceiptSpec{Argv: []string{"true"}, NoSnapshot: true},
		})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := cmdStatusTo([]string{"--url", server.URL, id}, &stdout, &stderr); code != 0 {
		t.Fatalf("status exit = %d; stderr=%q", code, stderr.String())
	}
	for _, want := range []string{
		"Source: empty workspace",
		"Workspace changes: unknown (retention incomplete)",
		"workspace changes not retained",
		"collection failed",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestWriteStatusShowsTruncatedWorkspaceChangeCount(t *testing.T) {
	var output bytes.Buffer
	writeStatus(&output, "runner", "runner/"+proto.NewULID(), proto.JobDetails{
		JobStatus: proto.JobStatus{State: proto.StateExited, Result: &proto.Result{
			ChangesOK: true, CleanupOK: true, LogsComplete: true,
			Changes: &proto.ChangeSummary{
				Paths: []string{"first"}, PathsTruncated: true, PathCount: 3, Bytes: 9,
			},
		}},
		Spec: proto.ReceiptSpec{Argv: []string{"true"}},
	})
	if !strings.Contains(output.String(), "… 2 more paths") {
		t.Fatalf("truncated status output = %q", output.String())
	}
}

func TestWriteStatusTreatsAmbiguousExecutionLogsAsUnknown(t *testing.T) {
	id := proto.NewULID()
	var output bytes.Buffer
	writeStatus(&output, "runner", "runner/"+id, proto.JobDetails{
		JobStatus: proto.JobStatus{ID: id, State: proto.StateAmbiguous, Result: &proto.Result{
			Started: false, ChangesOK: false, CleanupOK: true, LogsComplete: false,
			TransactionError: "execution state unknown",
		}},
		Spec: proto.ReceiptSpec{Argv: []string{"build"}},
	})

	for _, want := range []string{
		"Logs: availability unknown; attach to inspect retained logs",
		"errand attach runner/" + id,
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("ambiguous status output missing %q:\n%s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "process did not start") {
		t.Fatalf("ambiguous status made a definitive execution claim:\n%s", output.String())
	}
}
