package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func savedInterruptedApply(t *testing.T, peer, id, workspaceID string) string {
	t.Helper()
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	jobs := filepath.Join(stateHome, "errand", "jobs")
	if err := os.MkdirAll(jobs, 0700); err != nil {
		t.Fatal(err)
	}
	peerHash := sha256.Sum256([]byte(peer))
	path := filepath.Join(jobs, fmt.Sprintf("%x-%s.json", peerHash[:16], id))
	raw, _ := json.Marshal(map[string]any{"peer_url": peer, "job_id": id, "root": t.TempDir(),
		"manifest_root": (proto.Manifest{}).RootHash(), "submission_started": true,
		"apply_on_success": true, "automatic_apply": "applying", "workspace_id": workspaceID})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInspectionReportsInterruptedApplyWithoutChangingState(t *testing.T) {
	id := proto.NewULID()
	wsID := proto.NewULID()
	var unavailable atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("inspection wrote to runner: %s", r.Method)
			http.Error(w, "unexpected mutation", 400)
			return
		}
		if unavailable.Load() {
			http.Error(w, "unavailable", 503)
			return
		}
		switch r.URL.Path {
		case "/v0/jobs":
			json.NewEncoder(w).Encode([]proto.JobListEntry{})
		case "/v0/jobs/" + id:
			zero := 0
			json.NewEncoder(w).Encode(proto.JobDetails{JobStatus: proto.JobStatus{ID: id, State: proto.StateExited,
				Result: &proto.Result{ExitCode: &zero, ChangesOK: true}}, Spec: proto.ReceiptSpec{WorkspaceID: wsID}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	path := savedInterruptedApply(t, server.URL, id, wsID)
	before, _ := os.ReadFile(path)
	writeClientConfig(t, "")
	for _, offline := range []bool{false, true} {
		unavailable.Store(offline)
		for _, command := range []string{"ps", "status"} {
			var stdout, stderr bytes.Buffer
			args := []string{"--url", server.URL, "--json"}
			var code int
			if command == "ps" {
				code = cmdPsTo(args, &stdout, &stderr)
			} else {
				code = cmdStatusTo(append(args, id), &stdout, &stderr)
			}
			want := 0
			if offline {
				want = 1
			}
			if code != want || !json.Valid(stdout.Bytes()) || !strings.Contains(stdout.String(), "needs_recovery") {
				t.Fatalf("%s offline=%t: code=%d stdout=%s stderr=%s", command, offline, code, &stdout, &stderr)
			}
			if offline && (strings.Contains(stdout.String(), "admitted_at") || strings.Contains(stdout.String(), "\"spec\"")) {
				t.Fatalf("%s invented remote details: %s", command, &stdout)
			}
		}
	}
	checks := doctorApplyChecks()
	if len(checks) != 1 || !strings.Contains(checks[0].Hint, "fetch --apply "+server.URL+"/"+id) {
		t.Fatalf("doctor checks = %+v", checks)
	}
	// Explicit workspace and peer filters must not pull in unrelated applies.
	unavailable.Store(false)
	var stdout, stderr bytes.Buffer
	if code := cmdPsTo([]string{"--url", server.URL, "--workspace", proto.NewULID(), "--json"}, &stdout, &stderr); code != 0 || strings.Contains(stdout.String(), id) {
		t.Fatalf("filtered ps: %d %s %s", code, &stdout, &stderr)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("inspection changed apply state")
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_STATE_HOME"), "errand", "locks")); !os.IsNotExist(err) {
		t.Fatalf("inspection created locks: %v", err)
	}
}

func TestPsApplyGuidanceStaysWithItsRow(t *testing.T) {
	rows := []psRow{
		{Peer: "test", JobListEntry: proto.JobListEntry{ID: proto.NewULID(), State: proto.StateExited}, AutomaticApply: &client.AutomaticApplyStatus{State: "applied"}},
		{Peer: "test", JobListEntry: proto.JobListEntry{ID: proto.NewULID(), State: proto.StateExited}, AutomaticApply: &client.AutomaticApplyStatus{State: client.AutomaticApplyNeedsRecovery}},
	}
	var out bytes.Buffer
	writePsWithOptions(&out, rows, psRenderOptions{})
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || strings.Contains(lines[1], "fetch --apply") || !strings.Contains(lines[2], "fetch --apply test/"+rows[1].ID) {
		t.Fatalf("table guidance detached from row: %s", &out)
	}
	out.Reset()
	writePsWithOptions(&out, rows, psRenderOptions{interactive: true, width: 160})
	if strings.Count(out.String(), "fetch --apply") != 1 || strings.Contains(out.String(), "automatic apply: applied") {
		t.Fatalf("cards: %s", &out)
	}
}

func TestCLIStartupNeverScansApplyState(t *testing.T) {
	if raw := os.Getenv("ERRAND_INSPECTION_ARGS"); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			t.Fatal(err)
		}
		t.Setenv("XDG_STATE_HOME", "invalid-relative-state")
		os.Exit(runCLI(args))
	}
	for _, command := range []string{"", "ps", "status", "attach", "df", "peers", "workspaces", "config", "access", "setup", "serve", "version", "doctor", "fetch", "push", "kill", "gc"} {
		t.Run(command, func(t *testing.T) {
			args := []string{command, "--invalid-test-flag"}
			if command == "" {
				args = []string{"--help"}
			} else if command == "version" || command == "config" {
				args = []string{command}
			}
			raw, _ := json.Marshal(args)
			cmd := exec.Command(os.Args[0], "-test.run=^TestCLIStartupNeverScansApplyState$")
			cmd.Dir = t.TempDir()
			cmd.Env = append(os.Environ(), "ERRAND_INSPECTION_ARGS="+string(raw))
			out, err := cmd.CombinedOutput()
			if cmd.ProcessState == nil {
				t.Fatal(err)
			}
			if strings.Contains(string(out), "XDG_STATE_HOME") || strings.Contains(string(out), "resuming automatic") {
				t.Fatalf("startup touched apply state: %s", out)
			}
			if _, err := os.Stat(filepath.Join(cmd.Dir, "invalid-relative-state")); !os.IsNotExist(err) {
				t.Fatalf("startup created state: %v", err)
			}
		})
	}
}

func TestPsRecoveryPreservesMissingWorkspaceAndJobSemantics(t *testing.T) {
	var workspaceRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/jobs" {
			json.NewEncoder(w).Encode([]proto.JobListEntry{})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v0/workspaces/") {
			workspaceRequests.Add(1)
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	id := proto.NewULID()
	savedInterruptedApply(t, server.URL, id, proto.NewULID())
	for _, filter := range [][]string{{"--workspace", "missing"}, {}} {
		var out, errOut bytes.Buffer
		code := cmdPsTo(append([]string{"--url", server.URL}, filter...), &out, &errOut)
		if code != 0 || errOut.Len() != 0 {
			t.Fatalf("filter=%v code=%d out=%s err=%s", filter, code, &out, &errOut)
		}
		if len(filter) == 0 && (!strings.Contains(out.String(), "no longer retained") || strings.Contains(out.String(), "fetch --apply")) {
			t.Fatalf("missing receipt has misleading guidance: %s", &out)
		}
	}
	if workspaceRequests.Load() != 1 {
		t.Fatalf("workspace resolved %d times", workspaceRequests.Load())
	}
}

func TestPsDoesNotQueryJobsAfterListingFailure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	id := proto.NewULID()
	savedInterruptedApply(t, server.URL, id, "")
	var out, errOut bytes.Buffer
	if code := cmdPsTo([]string{"--url", server.URL, "--json"}, &out, &errOut); code != 1 || !strings.Contains(out.String(), id) {
		t.Fatalf("code=%d out=%s err=%s", code, &out, &errOut)
	}
	if requests.Load() != 1 {
		t.Fatalf("failed peer queried %d times", requests.Load())
	}
}

func TestPsDiscardsMalformedListingButKeepsLocalRecovery(t *testing.T) {
	workspace, otherWorkspace := proto.NewULID(), proto.NewULID()
	localID, otherID := proto.NewULID(), proto.NewULID()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": localID, "workspace_id": workspace, "state": "running", "command": "untrusted remote command"},
			{"id": otherID, "workspace_id": otherWorkspace, "state": "running", "admitted_at": "invalid timestamp"},
		})
	}))
	defer server.Close()
	savedInterruptedApply(t, server.URL, localID, workspace)
	var out, errOut bytes.Buffer
	code := cmdPsTo([]string{"--url", server.URL, "--workspace", workspace, "--json"}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "decoding response") {
		t.Fatalf("code=%d out=%s err=%s", code, &out, &errOut)
	}
	var rows []psRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != localID || rows[0].WorkspaceID != workspace || rows[0].State != "unknown" || !applyNeedsAttention(rows[0].AutomaticApply) {
		t.Fatalf("expected only locally known recovery state: %s", &out)
	}
	if strings.Contains(out.String(), "command") || strings.Contains(out.String(), "admitted_at") {
		t.Fatalf("rejected remote fields leaked into local row: %s", &out)
	}
	if requests.Load() != 1 {
		t.Fatalf("failed listing triggered %d requests", requests.Load())
	}
}

func TestDoctorRecoveryUsesConfiguredSSHHandle(t *testing.T) {
	writeClientConfig(t, "[peers.builder]\nssh = 'builder.test'\n")
	peer, _, err := resolvePeerTarget("", "builder")
	if err != nil {
		t.Fatal(err)
	}
	id := proto.NewULID()
	savedInterruptedApply(t, peer, id, "")
	checks := doctorApplyChecks()
	if len(checks) != 1 || !strings.Contains(checks[0].Hint, "fetch --apply builder/"+id) || strings.Contains(checks[0].Hint, "ssh://peer-") {
		t.Fatalf("checks = %+v", checks)
	}
}
