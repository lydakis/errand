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

func TestResolveApplyOnSuccessPrecedence(t *testing.T) {
	workspaceTrue := true
	workspaceFalse := false
	for name, tc := range map[string]struct {
		explicitApply   bool
		explicitNoApply bool
		workspace       *bool
		global          bool
		want            bool
		wantErr         bool
	}{
		"explicit apply":    {explicitApply: true, workspace: &workspaceFalse, want: true},
		"explicit no apply": {explicitNoApply: true, workspace: &workspaceTrue, global: true},
		"workspace true":    {workspace: &workspaceTrue, want: true},
		"workspace false":   {workspace: &workspaceFalse, global: true},
		"global":            {global: true, want: true},
		"safe default":      {},
		"contradictory":     {explicitApply: true, explicitNoApply: true, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := resolveApplyOnSuccess(
				tc.explicitApply, tc.explicitNoApply, tc.workspace, tc.global,
			)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("resolveApplyOnSuccess() = %t, %v", got, err)
			}
		})
	}
}

func TestCmdFetchRejectsConflictsWithoutApply(t *testing.T) {
	id := proto.NewULID()
	if code := cmdFetch([]string{"--conflicts", id}); code != 2 {
		t.Fatalf("fetch --conflicts exit = %d, want 2", code)
	}
}

func TestCmdGCRequiresExplicitTarget(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cmdGCTo(nil, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "cache|jobs|changes|all") {
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

func TestCmdGCChangesUsesExplicitLocalTarget(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := cmdGCTo([]string{"changes", "--older-than", "1d", "--dry-run"}, &stdout, &stderr); code != 0 ||
		!strings.Contains(stdout.String(), "local changes: would remove 0 records") {
		t.Fatalf("gc changes = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
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
		case "/v0/change-reconciliation":
			collectedCalls++
			if !proto.ValidChangeClientID(r.URL.Query().Get("client_id")) {
				t.Errorf("invalid change client ID %q", r.URL.Query().Get("client_id"))
			}
			json.NewEncoder(w).Encode(proto.ChangeReconciliationPage{JobIDs: []string{jobID}})
		case "/v0/change-reconciliation/ack":
			acknowledgementCalls++
			json.NewEncoder(w).Encode(proto.ChangeReconciliationAckResult{Acknowledged: 1})
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

func TestCmdGCAllDryRunIsObservationalAcrossEveryTarget(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	var cacheRequest proto.CacheGCRequest
	var jobRequest proto.JobGCRequest
	var reconciliationCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/cache/gc":
			if err := json.NewDecoder(r.Body).Decode(&cacheRequest); err != nil {
				t.Error(err)
			}
			json.NewEncoder(w).Encode(proto.CacheGCResult{RemovedBlobs: 2, FreedBytes: 10, DryRun: true})
		case "/v0/jobs/gc":
			if err := json.NewDecoder(r.Body).Decode(&jobRequest); err != nil {
				t.Error(err)
			}
			json.NewEncoder(w).Encode(proto.JobGCResult{SelectedJobs: 3, FreedBytes: 20, DryRun: true})
		case "/v0/change-reconciliation", "/v0/change-reconciliation/ack":
			reconciliationCalls++
			http.Error(w, "dry-run must not reconcile", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := cmdGCTo([]string{
		"all", "--url", server.URL, "--older-than", "1d", "--keep", "5", "--dry-run",
	}, &stdout, &stderr)
	if code != 0 || !cacheRequest.DryRun || !jobRequest.DryRun || reconciliationCalls != 0 ||
		!strings.Contains(stdout.String(), "cache: would remove 2") ||
		!strings.Contains(stdout.String(), "jobs: would remove 3") ||
		!strings.Contains(stdout.String(), "local changes: would remove 0") {
		t.Fatalf("gc all --dry-run = %d, cache=%+v jobs=%+v reconciliation=%d stdout=%q stderr=%q",
			code, cacheRequest, jobRequest, reconciliationCalls, stdout.String(), stderr.String())
	}
}

func TestCmdGCRoundsFractionalSecondsUp(t *testing.T) {
	var gotSeconds int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/change-reconciliation" {
			json.NewEncoder(w).Encode(proto.ChangeReconciliationPage{})
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
	if !strings.Contains(stdout.String(), "No active jobs found on reachable peers.") ||
		!strings.Contains(stderr.String(), "broken") {
		t.Fatalf("partial ps diagnostics missing; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCmdPsPresentsExplicitEmptyStates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `[]`)
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := cmdPsTo([]string{"--url", server.URL}, &stdout, &stderr); code != 0 {
		t.Fatalf("empty active ps exit = %d; stderr=%q", code, stderr.String())
	}
	if want := "No active jobs on " + server.URL + ".\n"; stdout.String() != want {
		t.Fatalf("empty active ps = %q, want %q", stdout.String(), want)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdPsTo([]string{"--url", server.URL, "--all"}, &stdout, &stderr); code != 0 {
		t.Fatalf("empty ps --all exit = %d; stderr=%q", code, stderr.String())
	}
	if want := "No jobs on " + server.URL + ".\n"; stdout.String() != want {
		t.Fatalf("empty ps --all = %q, want %q", stdout.String(), want)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdPsTo([]string{"--url", server.URL, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("empty ps --json exit = %d; stderr=%q", code, stderr.String())
	}
	var rows []psRow
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil || len(rows) != 0 {
		t.Fatalf("empty ps --json = %q, %v", stdout.String(), err)
	}
}

func TestCmdPsAggregatesConfiguredPeersNewestFirst(t *testing.T) {
	older := time.Date(2026, 8, 30, 14, 0, 0, 0, time.UTC)
	newer := older.Add(time.Minute)
	olderID := "01" + strings.Repeat("A", 24)
	newerID := "01" + strings.Repeat("B", 24)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	serveJob := func(peer, id string, admitted time.Time) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			started <- struct{}{}
			<-release
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]proto.JobListEntry{{
				ID: id, State: proto.StateRunning, AdmittedAt: admitted,
				Project: peer, Command: `"sleep" "10"`,
			}})
		}))
	}
	// Runner wall clocks disagree, so the newer ULID deliberately carries the
	// older admission timestamp. Cross-runner ordering must use the job ID.
	cabal := serveJob("atlas", newerID, older)
	defer cabal.Close()
	macMini := serveJob("errand", olderID, newer)
	defer macMini.Close()
	writeClientConfig(t, fmt.Sprintf(`default_peer = "cabal"
[peers.cabal]
url = %q
[peers.mac-mini]
url = %q
	`, cabal.URL, macMini.URL))

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- cmdPsTo(nil, &stdout, &stderr) }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("ps did not query configured peers concurrently")
		}
	}
	close(release)
	if code := <-done; code != 0 {
		t.Fatalf("ps exit = %d; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if strings.Count(out, "cabal") != 1 || strings.Count(out, "mac-mini") != 1 {
		t.Fatalf("ps did not aggregate configured peers: %q", out)
	}
	if strings.Index(out, "cabal") > strings.Index(out, "mac-mini") {
		t.Fatalf("ps rows are not globally newest-first: %q", out)
	}
}

func TestCmdPsDefaultsToActiveAndSupportsAllAndLast(t *testing.T) {
	base := time.Date(2026, 8, 30, 14, 0, 0, 0, time.UTC)
	var activeQueries int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("active") == "1" {
			activeQueries++
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]proto.JobListEntry{
			{ID: "01" + strings.Repeat("C", 24), State: proto.StateExited, AdmittedAt: base.Add(2 * time.Minute), Project: "newest-terminal", Command: `"false"`},
			{ID: "01" + strings.Repeat("B", 24), State: proto.StateRunning, AdmittedAt: base.Add(time.Minute), Project: "active", Command: `"sleep" "10"`},
			{ID: "01" + strings.Repeat("A", 24), State: proto.StateExited, AdmittedAt: base, Project: "old-terminal", Command: `"true"`},
		})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	if code := cmdPsTo([]string{"--url", server.URL}, &stdout, &stderr); code != 0 {
		t.Fatalf("active ps exit = %d; stderr=%q", code, stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "active") || strings.Contains(out, "terminal") {
		t.Fatalf("default ps did not restrict output to active jobs: %q", out)
	}
	if activeQueries != 1 {
		t.Fatalf("default ps made %d active-only list requests, want 1", activeQueries)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdPsTo([]string{"--url", server.URL, "--all"}, &stdout, &stderr); code != 0 {
		t.Fatalf("ps --all exit = %d; stderr=%q", code, stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "newest-terminal") || !strings.Contains(out, "old-terminal") {
		t.Fatalf("ps --all omitted terminal jobs: %q", out)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdPsTo([]string{"--url", server.URL, "--last", "1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("ps --last exit = %d; stderr=%q", code, stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "newest-terminal") || strings.Contains(out, "active") || strings.Contains(out, "old-terminal") {
		t.Fatalf("ps --last did not apply a global newest-first limit: %q", out)
	}
	if activeQueries != 1 {
		t.Fatalf("--all or --last unexpectedly used the active-only query: %d requests", activeQueries)
	}
}

func TestCmdPsRejectsLastBeyondListingWindow(t *testing.T) {
	writeClientConfig(t, "")
	var stdout, stderr bytes.Buffer
	if code := cmdPsTo([]string{"--last", "201"}, &stdout, &stderr); code != 2 {
		t.Fatalf("ps --last 201 exit = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "must not exceed 200") {
		t.Fatalf("ps --last 201 diagnostic = %q", stderr.String())
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
	if code := cmdPsTo([]string{"--url", server.URL, "--all"}, &stdout, &stderr); code != 0 {
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

func TestCmdInfoAggregatesConfiguredPeers(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	serveInfo := func(version string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			started <- struct{}{}
			<-release
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(proto.Info{Version: version})
		}))
	}
	cabal := serveInfo("linux-runner")
	defer cabal.Close()
	macMini := serveInfo("darwin-runner")
	defer macMini.Close()
	writeClientConfig(t, fmt.Sprintf(`[peers.cabal]
url = %q
[peers.mac-mini]
url = %q
	`, cabal.URL, macMini.URL))

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- cmdInfoTo(nil, &stdout, &stderr) }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("info did not query configured peers concurrently")
		}
	}
	close(release)
	if code := <-done; code != 0 {
		t.Fatalf("info exit = %d; stderr=%q", code, stderr.String())
	}
	var got map[string]proto.Info
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decoding aggregate info: %v; output=%q", err, stdout.String())
	}
	if got["cabal"].Version != "linux-runner" || got["mac-mini"].Version != "darwin-runner" {
		t.Fatalf("aggregate info = %+v", got)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdInfoTo([]string{"--on", "cabal"}, &stdout, &stderr); code != 0 {
		t.Fatalf("targeted info exit = %d; stderr=%q", code, stderr.String())
	}
	var targeted proto.Info
	if err := json.Unmarshal(stdout.Bytes(), &targeted); err != nil || targeted.Version != "linux-runner" {
		t.Fatalf("targeted info = %+v, %v; output=%q", targeted, err, stdout.String())
	}
}

func TestCmdDfAggregatesConfiguredPeersConcurrently(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	serveStorage := func(stats proto.StorageStats) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v0/storage" {
				http.NotFound(w, r)
				return
			}
			started <- struct{}{}
			<-release
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(stats)
		}))
	}
	cabal := serveStorage(proto.StorageStats{
		Cache: &proto.CacheStats{Blobs: 3, Bytes: 6 << 20, MaxBytes: 5 << 30, TTLHours: 336},
		Jobs:  proto.StorageCategory{Items: 7, Bytes: 1 << 30},
	})
	defer cabal.Close()
	macMini := serveStorage(proto.StorageStats{
		Cache: &proto.CacheStats{Blobs: 9, Bytes: 56 << 20, MaxBytes: 5 << 30, TTLHours: 336},
		Jobs:  proto.StorageCategory{Items: 2, Bytes: 84 << 20},
	})
	defer macMini.Close()
	writeClientConfig(t, fmt.Sprintf(`[peers.cabal]
url = %q
[peers.mac-mini]
url = %q
	`, cabal.URL, macMini.URL))

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- cmdDfTo(nil, &stdout, &stderr) }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("df did not query configured peers concurrently")
		}
	}
	close(release)
	if code := <-done; code != 0 {
		t.Fatalf("df exit = %d; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"LOCATION", "CACHE", "JOBS", "CHANGES", "TOTAL", "cabal", "mac-mini", "local", "6.0 MiB", "1.0 GiB", "56 MiB", "84 MiB"} {
		if !strings.Contains(out, want) {
			t.Fatalf("df output missing %q:\n%s", want, out)
		}
	}
}

func TestCmdDfJSONPreservesRawUsageAndNarrowsWithOn(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/storage" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(proto.StorageStats{
			Cache: &proto.CacheStats{Blobs: 3, Bytes: 42, MaxBytes: 100, TTLHours: 24},
			Jobs:  proto.StorageCategory{Items: 2, Bytes: 58},
		})
	}))
	defer server.Close()
	writeClientConfig(t, fmt.Sprintf(`[peers.cabal]
url = %q
	`, server.URL))

	var stdout, stderr bytes.Buffer
	if code := cmdDfTo([]string{"--on", "cabal", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("df --json exit = %d; stderr=%q", code, stderr.String())
	}
	var rows []dfRow
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("decoding df JSON: %v; output=%q", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"changes"`) {
		t.Fatalf("df JSON does not expose local changes: %s", stdout.String())
	}
	if len(rows) != 2 || rows[0].Location != "cabal" || rows[0].Cache == nil ||
		rows[0].Cache.Bytes != 42 || rows[0].Jobs.Bytes != 58 || rows[0].TotalBytes != 100 ||
		rows[1].Location != "local" || rows[1].Changes == nil {
		t.Fatalf("df JSON = %+v", rows)
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
	if len(fields) < 10 || fields[7] != "-" || fields[8] != "-" {
		t.Fatalf("queued ps row unexpectedly has process timing: %q", stdout.String())
	}
}

func TestCmdPsEscapesTerminalControlsInMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]proto.JobListEntry{{
			ID: proto.NewULID(), State: proto.StateRunning,
			Project: "atlas\r\x1b[2Jowned", GitCommit: "abc\t\x1b[2Jdef", Workdir: "build\n\x1b[2Jowned",
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
			DurationMS: 42000, Project: "atlas", GitCommit: "0123456789abcdef", GitDirty: true, Workdir: "vm",
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
		rows[0].DurationMS != 42000 || rows[0].Project != "atlas" || rows[0].GitCommit != "0123456789abcdef" ||
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
