package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

func writeClientConfig(t *testing.T, body string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	dir := filepath.Join(root, "errand")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveHandlePreservesRawURL(t *testing.T) {
	id := proto.NewULID()
	peerURL, label, jobID, err := resolveHandle("http://runner:9000/"+id, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if peerURL != "http://runner:9000" || label != peerURL || jobID != id {
		t.Fatalf("resolved raw handle = (%q, %q, %q), want port-preserving URL and %q", peerURL, label, jobID, id)
	}
}

func TestCmdRunRejectsWorkspaceRootWithoutSnapshot(t *testing.T) {
	if code := cmdRun([]string{"--no-snapshot", "--workspace-root", ".", "--", "/bin/true"}); code != 2 {
		t.Fatalf("no-snapshot workspace-root exit = %d, want 2", code)
	}
}

func TestCmdGCRequiresExplicitTarget(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cmdGCTo(nil, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "cache|jobs|outputs|all") {
		t.Fatalf("bare gc = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestCmdGCRejectsUnexpectedArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cmdGCTo([]string{"cache", "unexpected"}, &stdout, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "unexpected gc arguments") {
		t.Fatalf("gc extra args = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestParseRetentionDurationSupportsDays(t *testing.T) {
	got, err := parseRetentionDuration("30d")
	if err != nil || got != 30*24*time.Hour {
		t.Fatalf("parseRetentionDuration(30d) = %v, %v", got, err)
	}
}

func TestCmdGCAllComposesCacheAndJobEndpoints(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	jobID := proto.NewULID()
	var cacheCalls, jobCalls, collectedCalls, acknowledgementCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/cache/gc":
			cacheCalls++
			json.NewEncoder(w).Encode(proto.CacheGCResult{RemovedBlobs: 2, FreedBytes: 10})
		case "/v0/jobs/gc":
			jobCalls++
			var request proto.JobGCRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.OlderThanSeconds == nil || *request.OlderThanSeconds != 86400 ||
				request.Keep == nil || *request.Keep != 5 {
				t.Errorf("job GC request = %+v", request)
			}
			json.NewEncoder(w).Encode(proto.JobGCResult{
				SelectedJobs: 3, RemovedJobs: 3, FreedBytes: 20,
			})
		case "/v0/jobs/collected":
			collectedCalls++
			if !proto.ValidOutputClientID(r.URL.Query().Get("client_id")) {
				t.Errorf("invalid output client ID %q", r.URL.Query().Get("client_id"))
			}
			json.NewEncoder(w).Encode(proto.CollectedJobsPage{JobIDs: []string{jobID}})
		case "/v0/jobs/collected/ack":
			acknowledgementCalls++
			json.NewEncoder(w).Encode(proto.CollectedJobsAckResult{Acknowledged: 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	code := cmdGCTo([]string{"all", "--url", server.URL, "--older-than", "1d", "--keep", "5"}, &stdout, &stderr)
	if code != 0 || cacheCalls != 1 || jobCalls != 1 || collectedCalls != 1 || acknowledgementCalls != 1 ||
		!strings.Contains(stdout.String(), "cache: removed 2") ||
		!strings.Contains(stdout.String(), "jobs: removed 3") {
		t.Fatalf("gc all = %d, calls=(%d,%d,%d,%d), stdout=%q stderr=%q",
			code, cacheCalls, jobCalls, collectedCalls, acknowledgementCalls, stdout.String(), stderr.String())
	}
}

func TestCmdGCRoundsFractionalSecondsUp(t *testing.T) {
	var gotSeconds int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/jobs/collected" {
			json.NewEncoder(w).Encode(proto.CollectedJobsPage{})
			return
		}
		var request proto.JobGCRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.OlderThanSeconds != nil {
			gotSeconds = *request.OlderThanSeconds
		}
		json.NewEncoder(w).Encode(proto.JobGCResult{})
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	code := cmdGCTo([]string{"jobs", "--url", server.URL, "--older-than", "1500ms"}, &stdout, &stderr)
	if code != 0 || gotSeconds != 2 {
		t.Fatalf("gc fractional cutoff = %d, seconds=%d, stdout=%q stderr=%q",
			code, gotSeconds, stdout.String(), stderr.String())
	}
}

func TestCmdGCReportsCleanupFailuresSeparately(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(proto.JobGCResult{
			SelectedJobs: 1, RemovedJobs: 1, CleanupFailures: 1,
		})
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	code := cmdGCTo([]string{"jobs", "--url", server.URL, "--keep", "0"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stdout.String(), "removed 1") ||
		!strings.Contains(stdout.String(), "1 cleanup failures") {
		t.Fatalf("gc cleanup failure = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestResolvePeerTargetUsesConfiguredDefaultAlias(t *testing.T) {
	writeClientConfig(t, `default_peer = "cabal"
[peers.cabal]
url = "http://cabal:7443"
`)
	peerURL, label, err := resolvePeerTarget("", "")
	if err != nil {
		t.Fatal(err)
	}
	if peerURL != "http://cabal:7443" || label != "cabal" {
		t.Fatalf("default peer target = (%q, %q), want configured alias and URL", peerURL, label)
	}
}

func TestResolvePeerTargetRejectsConflictingSelectors(t *testing.T) {
	if _, _, err := resolvePeerTarget("http://runner:7443", "cabal"); err == nil {
		t.Fatal("--url and --on were accepted together")
	}
}

func TestResolveHandleFailsClosed(t *testing.T) {
	writeClientConfig(t, `default_peer = "cabal"
[peers.cabal]
url = "http://cabal:7443"
`)
	id := proto.NewULID()
	for name, tc := range map[string][3]string{
		"unknown alias":    {"cabla/" + id, "", ""},
		"conflicting on":   {"cabal/" + id, "", "other"},
		"conflicting urls": {"http://atlas:7443/" + id, "http://cabal:7443", ""},
		"invalid job id":   {"cabal/not-a-ulid", "", ""},
		"dual selectors":   {id, "http://runner:7443", "cabal"},
	} {
		t.Run(name, func(t *testing.T) {
			args, rawURL, on := tc[0], tc[1], tc[2]
			if _, _, _, err := resolveHandle(args, rawURL, on); err == nil {
				t.Fatalf("resolveHandle(%q, %q, %q) succeeded", args, rawURL, on)
			}
		})
	}
}

func TestResolveHandleMatchingURLSelector(t *testing.T) {
	id := proto.NewULID()
	peerURL, label, jobID, err := resolveHandle(
		"http://runner:9000/"+id, "http://runner:9000/", "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if peerURL != "http://runner:9000" || label != peerURL || jobID != id {
		t.Fatalf("matching URL selector resolved to (%q, %q, %q)", peerURL, label, jobID)
	}
}

func TestResolveHandleURLOverrideUsesEffectiveURLLabel(t *testing.T) {
	id := proto.NewULID()
	peerURL, label, jobID, err := resolveHandle("cabal/"+id, "http://runner:9000", "")
	if err != nil {
		t.Fatal(err)
	}
	if peerURL != "http://runner:9000" || label != peerURL || jobID != id {
		t.Fatalf("URL override resolved to (%q, %q, %q)", peerURL, label, jobID)
	}
}

func TestCmdPsReportsPartialConfiguredPeerFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `[]`)
	}))
	defer server.Close()
	writeClientConfig(t, fmt.Sprintf(`default_peer = "good"
[peers.good]
url = %q
[peers.broken]
url = ""
`, server.URL))
	var stdout, stderr bytes.Buffer
	if code := cmdPsTo(nil, &stdout, &stderr); code == 0 {
		t.Fatalf("partial ps returned success; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "PEER") || !strings.Contains(stderr.String(), "broken") {
		t.Fatalf("partial ps diagnostics missing; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCmdPsPresentsTimingSourceAndWorkdir(t *testing.T) {
	admitted := time.Date(2026, 8, 30, 14, 0, 0, 0, time.Local)
	started := time.Date(2026, 8, 30, 14, 5, 6, 0, time.Local)
	finished := started.Add(99 * time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]proto.JobListEntry{{
			ID:           proto.NewULID(),
			State:        proto.StateExited,
			Command:      `"nix" "build"`,
			AdmittedAt:   admitted,
			StartedAt:    &started,
			FinishedAt:   &finished,
			DurationMS:   123000,
			GitCommit:    "0123456789abcdef0123456789abcdef01234567",
			GitDirty:     true,
			ManifestRoot: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			Workdir:      "build",
		}})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := cmdPsTo([]string{"--url", server.URL}, &stdout, &stderr); code != 0 {
		t.Fatalf("ps exit = %d; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"ADMITTED", "STARTED", "DURATION", "SOURCE", "WORKDIR",
		"2026-08-30 14:00:00", "2026-08-30 14:05:06", "2m3s", "0123456789ab+dirty", "build",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("ps output missing %q:\n%s", want, out)
		}
	}
}

func TestCmdPsQueuedJobHasAdmissionButNoProcessTiming(t *testing.T) {
	admitted := time.Date(2026, 8, 30, 14, 0, 0, 0, time.Local)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]proto.JobListEntry{{
			ID: proto.NewULID(), State: proto.StateQueued, AdmittedAt: admitted,
			Command: `"nix" "build"`,
		}})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := cmdPsTo([]string{"--url", server.URL}, &stdout, &stderr); code != 0 {
		t.Fatalf("ps exit = %d; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "queued") || !strings.Contains(out, "2026-08-30 14:00:00") {
		t.Fatalf("queued ps row lacks state or admission time: %q", stdout.String())
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("queued ps output has %d lines, want header and row: %q", len(lines), out)
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 10 || fields[6] != "-" || fields[7] != "-" {
		t.Fatalf("queued ps row unexpectedly has process timing: %q", stdout.String())
	}
}

func TestCmdPsEscapesTerminalControlsInMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]proto.JobListEntry{{
			ID: proto.NewULID(), State: proto.StateRunning,
			GitCommit: "abc\t\x1b[2Jdef", Workdir: "build\n\x1b[2Jowned",
		}})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := cmdPsTo([]string{"--url", server.URL}, &stdout, &stderr); code != 0 {
		t.Fatalf("ps exit = %d; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	for _, unsafe := range []string{"\t", "\n\x1b", "\x1b"} {
		if strings.Contains(out, unsafe) {
			t.Fatalf("ps output contains raw terminal control %q: %q", unsafe, out)
		}
	}
	for _, escaped := range []string{`\t`, `\n`, `\x1b`} {
		if !strings.Contains(out, escaped) {
			t.Fatalf("ps output does not visibly escape %q: %q", escaped, out)
		}
	}
}

func TestCmdPsJSONIncludesPeerAndReceiptMetadata(t *testing.T) {
	started := time.Date(2026, 8, 30, 14, 5, 6, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]proto.JobListEntry{{
			ID: proto.NewULID(), State: proto.StateRunning, StartedAt: &started,
			DurationMS: 42000, GitCommit: "0123456789abcdef", GitDirty: true, Workdir: "vm",
		}})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := cmdPsTo([]string{"--url", server.URL, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("ps --json exit = %d; stderr=%q", code, stderr.String())
	}
	var rows []struct {
		Peer string `json:"peer"`
		proto.JobListEntry
	}
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("decoding ps JSON: %v; output=%q", err, stdout.String())
	}
	if len(rows) != 1 || rows[0].Peer != server.URL || rows[0].StartedAt == nil ||
		rows[0].DurationMS != 42000 || rows[0].GitCommit != "0123456789abcdef" ||
		!rows[0].GitDirty || rows[0].Workdir != "vm" {
		t.Fatalf("ps JSON = %+v", rows)
	}
}

func TestCmdRunRejectsConflictingSnapshotFlags(t *testing.T) {
	if code := cmdRun([]string{"--no-snapshot", "--include-all", "--", "/bin/true"}); code != 2 {
		t.Fatalf("conflicting snapshot flags exit = %d, want 2", code)
	}
}

func TestCmdRunRejectsWorkdirWithoutSnapshot(t *testing.T) {
	if code := cmdRun([]string{"--no-snapshot", "--workdir", "build", "--", "/bin/true"}); code != 2 {
		t.Fatalf("no-snapshot workdir exit = %d, want 2", code)
	}
}
