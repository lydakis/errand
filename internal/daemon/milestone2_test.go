package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

// TestReconciliationSettlesOrphanedJob simulates a daemon crash: a job
// directory with a persisted scope record and a still-running scoped
// process, but no result. A fresh daemon must terminate the survivor,
// clean the workspace, and write a durable ambiguous result — without
// replaying anything.
func TestReconciliationSettlesOrphanedJob(t *testing.T) {
	stateDir := t.TempDir()
	id := proto.NewULID()
	dir := filepath.Join(stateDir, "jobs", id)
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/bin/sleep", "30"},
		Limits: proto.DefaultLimits(),
	}
	writeReceipt := func(name string, v any) {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeReceipt("spec.json", proto.NewReceiptSpec(spec))
	writeReceipt("admission.json", proto.Admission{Method: "insecure-test", Time: time.Now()})

	token := strings.Repeat("ab", 16)
	writeReceipt("scope.json", scopeRecord{Token: token})

	orphan := exec.Command("/bin/sleep", "30")
	orphan.Dir = workspace
	orphan.Env = []string{processScopeEnv + "=" + token}
	orphan.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	pid := orphan.Process.Pid
	go orphan.Wait() // reap so a killed orphan reads as gone, not zombie

	d, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	d.mu.Lock()
	j := d.jobs[id]
	d.mu.Unlock()
	if j == nil || j.state != proto.StateAmbiguous {
		t.Fatalf("reconciled job state = %v, want ambiguous", j)
	}
	if j.result == nil || j.result.State != proto.StateAmbiguous ||
		!strings.Contains(j.result.TransactionError, "not replayed") {
		t.Fatalf("reconciled result = %+v", j.result)
	}
	if !strings.Contains(j.result.TransactionError, "surviving processes terminated") {
		t.Fatalf("result does not report the terminated survivor: %+v", j.result)
	}
	if _, err := os.ReadFile(filepath.Join(dir, "result.json")); err != nil {
		t.Fatalf("reconciliation left no durable result: %v", err)
	}
	if _, err := os.Lstat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace survived reconciliation: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "scope.json")); !os.IsNotExist(err) {
		t.Fatalf("scope record survived reconciliation: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("orphan pid %d survived reconciliation: %v", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestReconciliationRetainsMalformedScopeRecordAndReportsCleanupFailure(t *testing.T) {
	stateDir := t.TempDir()
	id := proto.NewULID()
	dir := filepath.Join(stateDir, "jobs", id)
	if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{V: proto.ProtoVersion, Argv: []string{"/bin/true"}, Limits: proto.DefaultLimits()}
	for name, value := range map[string]any{
		"spec.json":  proto.NewReceiptSpec(spec),
		"scope.json": scopeRecord{Token: "not-a-production-token"},
	} {
		b, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	d, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.mu.Lock()
	j := d.jobs[id]
	d.mu.Unlock()
	if j == nil || j.result == nil || j.result.CleanupOK {
		t.Fatalf("malformed-scope reconciliation result = %+v", j)
	}
	if !strings.Contains(j.result.TransactionError, "process scope token") {
		t.Fatalf("malformed-scope transaction error = %q", j.result.TransactionError)
	}
	if _, err := os.Lstat(filepath.Join(dir, "scope.json")); err != nil {
		t.Fatalf("malformed scope record was not retained: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "workspace")); err != nil {
		t.Fatalf("workspace recovery evidence was not retained: %v", err)
	}
}

func TestRestartRetriesRetainedScopeForTerminalJob(t *testing.T) {
	stateDir := t.TempDir()
	id := proto.NewULID()
	dir := filepath.Join(stateDir, "jobs", id)
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("ab", 16)
	write := func(name string, value any) []byte {
		b, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
		return b
	}
	spec := proto.Spec{V: proto.ProtoVersion, Argv: []string{"/bin/sleep", "30"}, Limits: proto.DefaultLimits()}
	write("spec.json", proto.NewReceiptSpec(spec))
	write("admission.json", proto.Admission{Method: "insecure-test", Time: time.Now()})
	write("scope.json", scopeRecord{Token: token})
	zero := 0
	resultBefore := write("result.json", proto.Result{
		State: proto.StateExited, Started: true, ExitCode: &zero,
		ChangesOK: true, CleanupOK: false, LogsComplete: true,
		TransactionError: "process scope cleanup incomplete; recovery record retained",
	})

	orphan := exec.Command("/bin/sleep", "30")
	orphan.Dir = workspace
	orphan.Env = []string{processScopeEnv + "=" + token}
	orphan.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	pid := orphan.Process.Pid
	go orphan.Wait()

	d, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal job survivor pid %d was not recovered", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Lstat(workspace); !os.IsNotExist(err) {
		t.Fatalf("terminal job workspace survived successful recovery: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "scope.json")); !os.IsNotExist(err) {
		t.Fatalf("terminal job scope record survived successful recovery: %v", err)
	}
	resultAfter, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(resultBefore, resultAfter) {
		t.Fatal("restart recovery rewrote the immutable terminal result")
	}
}

func TestFinalizeIncludesScopeRecordRemovalInCleanupResult(t *testing.T) {
	stateDir := t.TempDir()
	id := proto.NewULID()
	dir := filepath.Join(stateDir, "jobs", id)
	if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A non-empty directory at scope.json makes os.Remove fail portably.
	if err := os.Mkdir(filepath.Join(dir, "scope.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scope.json", "sentinel"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{cfg: Config{StateDir: stateDir}, running: map[string]*Job{}}
	j := &Job{ID: id, Dir: dir, state: proto.StateRunning, done: make(chan struct{}), logReady: make(chan struct{})}
	d.running[j.ID] = j
	code := 0
	res := &proto.Result{Started: true, ExitCode: &code, ChangesOK: true, CleanupOK: true, LogsComplete: true}
	j.finalize(d, res, false)
	if res.CleanupOK {
		t.Fatalf("scope record removal failure left cleanup_ok=true: %+v", res)
	}
	if !strings.Contains(res.TransactionError, "removing process scope record") {
		t.Fatalf("scope removal failure was omitted from transaction error: %+v", res)
	}
	var persisted proto.Result
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.CleanupOK || !strings.Contains(persisted.TransactionError, "removing process scope record") {
		t.Fatalf("persisted cleanup result = %+v", persisted)
	}
}

func TestFinalizeRetainsScopeRecordAfterProcessCleanupFailure(t *testing.T) {
	stateDir := t.TempDir()
	id := proto.NewULID()
	dir := filepath.Join(stateDir, "jobs", id)
	if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	scopePath := filepath.Join(dir, "scope.json")
	if err := os.WriteFile(scopePath, []byte(`{"token":"`+strings.Repeat("ab", 16)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{cfg: Config{StateDir: stateDir}, running: map[string]*Job{}}
	j := &Job{ID: id, Dir: dir, state: proto.StateRunning, done: make(chan struct{}), logReady: make(chan struct{})}
	d.running[j.ID] = j
	code := 0
	res := &proto.Result{Started: true, ExitCode: &code, ChangesOK: true, CleanupOK: false, LogsComplete: true}
	j.finalizeWithScopeOutcome(d, res, false, false)
	if _, err := os.Lstat(scopePath); err != nil {
		t.Fatalf("failed process cleanup lost its recovery locator: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "workspace")); err != nil {
		t.Fatalf("failed process cleanup lost its workspace evidence: %v", err)
	}
	if res.CleanupOK || !strings.Contains(res.TransactionError, "scope cleanup incomplete") {
		t.Fatalf("failed process cleanup result = %+v", res)
	}
}

// TestReconciliationWithoutScopeRecordStillSettles covers the crash window
// before a scope record exists: no processes to hunt, but the receipt must
// still become durably ambiguous and the workspace must go.
func TestReconciliationWithoutScopeRecordStillSettles(t *testing.T) {
	stateDir := t.TempDir()
	id := proto.NewULID()
	dir := filepath.Join(stateDir, "jobs", id)
	if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{V: proto.ProtoVersion, Argv: []string{"/bin/true"}, Limits: proto.DefaultLimits()}
	b, _ := json.MarshalIndent(proto.NewReceiptSpec(spec), "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "spec.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.mu.Lock()
	j := d.jobs[id]
	d.mu.Unlock()
	if j == nil || j.state != proto.StateAmbiguous || j.result == nil || !j.result.CleanupOK {
		t.Fatalf("scope-less reconciliation job = %+v result = %+v", j, j.result)
	}
	if _, err := os.Lstat(filepath.Join(dir, "workspace")); !os.IsNotExist(err) {
		t.Fatalf("workspace survived reconciliation: %v", err)
	}
}

func TestListJobsNewestFirst(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"f": "x"})
	first := proto.NewULID()
	resp := rawSubmit(t, ts.URL, first, root, []string{"/bin/echo", "one"})
	resp.Body.Close()
	waitTerminal(t, ts.URL, first)
	second := proto.NewULID()
	resp = rawSubmit(t, ts.URL, second, root, []string{"/bin/echo", "two"})
	resp.Body.Close()
	waitTerminal(t, ts.URL, second)

	listResp, err := http.Get(ts.URL + "/v0/jobs")
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()
	var entries []proto.JobListEntry
	if err := json.NewDecoder(listResp.Body).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].ID != second || entries[1].ID != first {
		t.Fatalf("listing = %+v, want [%s %s]", entries, second, first)
	}
	if entries[0].Command == "" || entries[0].AdmittedAt.IsZero() {
		t.Fatalf("listing entry lacks command or admission time: %+v", entries[0])
	}
	if entries[0].StartedAt == nil || entries[0].FinishedAt == nil ||
		entries[0].FinishedAt.Before(*entries[0].StartedAt) {
		t.Fatalf("listing entry lacks valid process timing: %+v", entries[0])
	}
	if entries[0].ExitCode == nil || *entries[0].ExitCode != 0 {
		t.Fatalf("listing entry lacks exit code: %+v", entries[0])
	}
}

func TestActiveJobListFiltersBeforeReceiptCap(t *testing.T) {
	d, ts := testDaemon(t)
	activeID := "01" + strings.Repeat("0", 24)
	d.mu.Lock()
	d.jobs[activeID] = &Job{
		ID: activeID, Spec: proto.Spec{Argv: []string{"/bin/sleep", "10"}},
		Admission: proto.Admission{Time: time.Now().Add(-time.Hour)}, state: proto.StateRunning,
	}
	for i := 0; i < 201; i++ {
		id := proto.NewULID()
		d.jobs[id] = &Job{
			ID: id, Spec: proto.Spec{Argv: []string{"/bin/true"}},
			Admission: proto.Admission{Time: time.Now()}, state: proto.StateExited,
		}
	}
	d.mu.Unlock()

	entries, err := client.ListActive(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != activeID {
		t.Fatalf("active listing = %+v, want only %s", entries, activeID)
	}
}

func TestJobListSummaryIsByteBounded(t *testing.T) {
	longArg := strings.Repeat("\u0000\"\\界", 1<<16)
	// NUL has the largest JSON expansion (six bytes), so this exercises the
	// actual worst case for the fixed listing response budget.
	longMetadata := strings.Repeat("\x00", 1<<16)
	original := []string{"/bin/tool", longArg, "tail"}
	j := &Job{
		ID: proto.NewULID(), Spec: proto.Spec{
			Argv: original, Workdir: longMetadata,
			GitCommit: longMetadata, ManifestRoot: longMetadata,
		}, state: proto.StateRunning, startedAt: time.Now().Add(-time.Hour),
		Admission: proto.Admission{Time: time.Now(), Project: longMetadata},
	}
	entry := j.summary()
	if !entry.CommandTruncated || !entry.WorkdirTruncated || !entry.ProjectTruncated ||
		!entry.GitCommitTruncated || !entry.ManifestRootTruncated {
		t.Fatalf("large listing metadata was not marked truncated: %+v", entry)
	}
	if !strings.HasSuffix(entry.Command, "…") || !utf8.ValidString(entry.Command) {
		t.Fatalf("bounded command is not valid marked UTF-8: %q", entry.Command)
	}
	if len(entry.Command) > maxListCommandBytes {
		t.Fatalf("bounded command is %d bytes, want <= %d", len(entry.Command), maxListCommandBytes)
	}
	if len(entry.Workdir) > maxListWorkdirBytes || len(entry.Project) > maxListProjectBytes ||
		len(entry.GitCommit) > maxListDigestBytes ||
		len(entry.ManifestRoot) > maxListDigestBytes {
		t.Fatalf("listing metadata exceeds bounds: %+v", entry)
	}
	if len(j.Spec.Argv) != 3 || j.Spec.Argv[1] != longArg {
		t.Fatal("building a listing summary mutated the admitted argv")
	}

	entries := make([]proto.JobListEntry, 200)
	for i := range entries {
		entries[i] = entry
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxListResponseBytes {
		t.Fatalf("maximum listing encoded to %d bytes, want <= %d", len(encoded), maxListResponseBytes)
	}
}

func TestJobListSummaryIncludesTimingAndSourceContext(t *testing.T) {
	started := time.Date(2026, 8, 30, 14, 5, 6, 0, time.UTC)
	finished := started.Add(3 * time.Minute)
	j := &Job{
		ID: proto.NewULID(),
		Spec: proto.Spec{
			Argv:         []string{"nix", "build"},
			Workdir:      "vm",
			ManifestRoot: strings.Repeat("a", 64),
			GitCommit:    strings.Repeat("b", 40),
			GitDirty:     true,
		},
		Admission: proto.Admission{Time: started.Add(-time.Second), Project: "atlas"},
		state:     proto.StateExited,
		startedAt: started,
		result: &proto.Result{
			State: proto.StateExited, StartedAt: &started, FinishedAt: &finished,
			DurationMS: 180000,
		},
	}

	entry := j.summary()
	if entry.StartedAt == nil || !entry.StartedAt.Equal(started) ||
		entry.FinishedAt == nil || !entry.FinishedAt.Equal(finished) ||
		entry.DurationMS != 180000 || entry.Workdir != "vm" || entry.Project != "atlas" ||
		entry.ManifestRoot != strings.Repeat("a", 64) ||
		entry.GitCommit != strings.Repeat("b", 40) || !entry.GitDirty {
		t.Fatalf("listing context = %+v", entry)
	}
}

func TestRunningJobListSummaryComputesElapsedTimeOnRunner(t *testing.T) {
	started := time.Now().Add(-2 * time.Second)
	j := &Job{state: proto.StateRunning, startedAt: started}
	entry := j.summary()
	if entry.DurationMS < 1900 || entry.DurationMS > 3000 {
		t.Fatalf("runner elapsed time = %dms, want about 2000ms", entry.DurationMS)
	}
}

func TestQueuedJobListSummaryHasNoProcessTiming(t *testing.T) {
	j := &Job{
		state:     proto.StateQueued,
		Admission: proto.Admission{Time: time.Now().Add(-time.Minute)},
	}
	entry := j.summary()
	if entry.StartedAt != nil || entry.FinishedAt != nil || entry.DurationMS != 0 {
		t.Fatalf("queued job has process timing: %+v", entry)
	}
}

func TestJobListSummaryEscapesArgumentBoundariesAndTerminalControls(t *testing.T) {
	j := &Job{Spec: proto.Spec{Argv: []string{"tool", "two words", "line\nnext", "\t\r\x1b[2J"}}}
	entry := j.summary()
	for _, unsafe := range []string{"\n", "\t", "\r", "\x1b"} {
		if strings.Contains(entry.Command, unsafe) {
			t.Fatalf("command preview contains raw terminal control %q: %q", unsafe, entry.Command)
		}
	}
	for _, want := range []string{`"tool"`, `"two words"`, `"line\nnext"`, `"\t\r\x1b[2J"`} {
		if !strings.Contains(entry.Command, want) {
			t.Fatalf("escaped command preview %q does not contain %q", entry.Command, want)
		}
	}
}

func TestListManyLargeJobsStaysDecodable(t *testing.T) {
	d, ts := testDaemon(t)
	longArg := strings.Repeat("\u0000\"\\界", 1<<16)
	ids := make([]string, 200)
	d.mu.Lock()
	for i := range ids {
		ids[i] = proto.NewULID()
		d.jobs[ids[i]] = &Job{
			ID: ids[i], Spec: proto.Spec{Argv: []string{"/bin/tool", longArg}},
			Admission: proto.Admission{Time: time.Now()}, state: proto.StateRunning,
		}
	}
	d.mu.Unlock()
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))

	entries, err := client.List(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(ids) {
		t.Fatalf("large listing rows = %d, want %d", len(entries), len(ids))
	}
	for i, entry := range entries {
		if entry.ID != ids[i] {
			t.Fatalf("large listing row %d id = %s, want %s", i, entry.ID, ids[i])
		}
		if !entry.CommandTruncated || len(entry.Command) > maxListCommandBytes {
			t.Fatalf("large listing row %d is not bounded: %+v", i, entry)
		}
	}
}

func TestDetachAttachRoundTrip(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"f.txt": "payload\n"})
	var submitOut, submitErr bytes.Buffer
	code := client.Run(client.RunOptions{
		PeerURL: ts.URL, PeerName: "testpeer", Root: root,
		Argv:   []string{"/bin/sh", "-c", "sleep 0.3; cat f.txt; exit 4"},
		Detach: true, Stdout: &submitOut, Stderr: &submitErr,
	})
	if code != 0 {
		t.Fatalf("detached submit exit = %d; stderr: %s", code, submitErr.String())
	}
	handle := strings.TrimSpace(submitOut.String())
	if !strings.HasPrefix(handle, "testpeer/") {
		t.Fatalf("detached stdout = %q, want a testpeer/ULID handle", handle)
	}
	jobID := strings.TrimPrefix(handle, "testpeer/")
	if !proto.ValidULID(jobID) {
		t.Fatalf("handle job id %q is not a ULID", jobID)
	}

	var out, attachErr bytes.Buffer
	acode := client.Attach(client.AttachOptions{
		PeerURL: ts.URL, PeerName: "testpeer", JobID: jobID,
		Stdout: &out, Stderr: &attachErr,
	})
	if acode != 4 {
		t.Fatalf("attach exit = %d, want the remote 4; stderr: %s", acode, attachErr.String())
	}
	if !strings.Contains(out.String(), "payload") {
		t.Fatalf("attach missed the job output: %q", out.String())
	}

	// A second attach to the now-terminal job replays the full log.
	var replay bytes.Buffer
	if again := client.Attach(client.AttachOptions{
		PeerURL: ts.URL, JobID: jobID, Stdout: &replay, Stderr: &bytes.Buffer{},
	}); again != 4 {
		t.Fatalf("terminal re-attach exit = %d, want 4", again)
	}
	if !strings.Contains(replay.String(), "payload") {
		t.Fatalf("terminal re-attach did not replay logs: %q", replay.String())
	}
}

func TestAttachToUnknownJobFails(t *testing.T) {
	_, ts := testDaemon(t)
	var out, errb bytes.Buffer
	code := client.Attach(client.AttachOptions{
		PeerURL: ts.URL, JobID: proto.NewULID(), Stdout: &out, Stderr: &errb,
	})
	if code != client.ExitTransaction || !strings.Contains(errb.String(), "no such job") {
		t.Fatalf("unknown-job attach = %d, stderr %q", code, errb.String())
	}
}
