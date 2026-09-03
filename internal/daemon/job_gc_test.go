package daemon

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
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

func addGCJob(t *testing.T, d *Daemon, state string, settledAt time.Time, cleanupOK bool) *Job {
	t.Helper()
	id := proto.NewULID()
	dir := filepath.Join(d.jobsDir(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	result := &proto.Result{
		State: state, SettledAt: &settledAt,
		ChangesOK: true, CleanupOK: cleanupOK, LogsComplete: true,
	}
	if err := replaceJSON(filepath.Join(dir, "result.json"), result); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "io.log"), []byte("receipt data"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	j := &Job{ID: id, Dir: dir, state: state, result: result, done: done}
	d.mu.Lock()
	d.jobs[id] = j
	d.mu.Unlock()
	return j
}

func postJobGC(t *testing.T, url string, request proto.JobGCRequest) proto.JobGCResult {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url+"/v0/jobs/gc", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("job GC = %s", resp.Status)
	}
	var result proto.JobGCResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestJobGCCombinesAgeAndKeepProtections(t *testing.T) {
	d, ts := testDaemon(t)
	clientID := "0123456789abcdef0123456789abcdef"
	now := time.Now()
	oldest := addGCJob(t, d, proto.StateExited, now.Add(-72*time.Hour), true)
	older := addGCJob(t, d, proto.StateKilled, now.Add(-48*time.Hour), true)
	for _, job := range []*Job{oldest, older} {
		job.Spec.ChangeClientID = clientID
		job.result.Changes = &proto.ChangeSummary{Paths: []string{"artifact"}, PathCount: 1, BundleRoot: strings.Repeat("a", 64)}
	}
	newest := addGCJob(t, d, proto.StateExited, now.Add(-36*time.Hour), true)
	addGCJob(t, d, proto.StateAmbiguous, now.Add(-96*time.Hour), true)
	addGCJob(t, d, proto.StateExited, now.Add(-96*time.Hour), false)

	seconds := int64((24 * time.Hour) / time.Second)
	keep := 1
	dry := postJobGC(t, ts.URL, proto.JobGCRequest{
		OlderThanSeconds: &seconds, Keep: &keep, DryRun: true,
	})
	if dry.SelectedJobs != 2 || dry.RemovedJobs != 0 || dry.FreedBytes == 0 ||
		dry.ProtectedJobs != 2 || !dry.DryRun {
		t.Fatalf("dry-run result = %+v", dry)
	}
	for _, j := range []*Job{oldest, older, newest} {
		if _, err := os.Stat(j.Dir); err != nil {
			t.Fatalf("dry-run removed %s: %v", j.ID, err)
		}
	}

	result := postJobGC(t, ts.URL, proto.JobGCRequest{OlderThanSeconds: &seconds, Keep: &keep})
	if result.SelectedJobs != 2 || result.RemovedJobs != 2 || result.FailedJobs != 0 {
		t.Fatalf("GC result = %+v", result)
	}
	resp, err := http.Get(ts.URL + "/v0/change-reconciliation?client_id=" + clientID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page proto.ChangeReconciliationPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	removed := map[string]bool{}
	for _, id := range page.JobIDs {
		removed[id] = true
	}
	if len(removed) != 2 || !removed[oldest.ID] || !removed[older.ID] {
		t.Fatalf("durable collected job IDs = %v, want %s and %s", page.JobIDs, oldest.ID, older.ID)
	}
	for _, j := range []*Job{oldest, older} {
		if _, err := os.Stat(j.Dir); !os.IsNotExist(err) {
			t.Fatalf("collected receipt %s still exists: %v", j.ID, err)
		}
	}
	if _, err := os.Stat(newest.Dir); err != nil {
		t.Fatalf("newest retained receipt is missing: %v", err)
	}
}

func TestJobGCProtectsActiveLogReader(t *testing.T) {
	d, ts := testDaemon(t)
	j := addGCJob(t, d, proto.StateExited, time.Now().Add(-48*time.Hour), true)
	if !j.acquireLogReader() {
		t.Fatal("could not acquire test log reader")
	}
	defer j.releaseLogReader()
	keep := 0
	result := postJobGC(t, ts.URL, proto.JobGCRequest{Keep: &keep})
	if result.SelectedJobs != 0 || result.ProtectedJobs != 1 {
		t.Fatalf("GC with active reader = %+v", result)
	}
	if _, err := os.Stat(j.Dir); err != nil {
		t.Fatalf("active reader receipt was removed: %v", err)
	}
}

func TestJobGCProtectsTransactionIncompleteReceipts(t *testing.T) {
	d, ts := testDaemon(t)
	logs := addGCJob(t, d, proto.StateExited, time.Now().Add(-48*time.Hour), true)
	changes := addGCJob(t, d, proto.StateExited, time.Now().Add(-48*time.Hour), true)
	transaction := addGCJob(t, d, proto.StateKilled, time.Now().Add(-48*time.Hour), true)
	logs.result.LogsComplete = false
	changes.result.ChangesOK = false
	transaction.result.TransactionError = "persisting logs failed"

	keep := 0
	result := postJobGC(t, ts.URL, proto.JobGCRequest{Keep: &keep})
	if result.SelectedJobs != 0 || result.ProtectedJobs != 3 || result.RemovedJobs != 0 {
		t.Fatalf("incomplete receipt GC = %+v", result)
	}
	for _, j := range []*Job{logs, changes, transaction} {
		if _, err := os.Stat(j.Dir); err != nil {
			t.Fatalf("incomplete receipt %s was removed: %v", j.ID, err)
		}
	}
}

func TestRemoveJobReceiptDistinguishesRemovedSkippedAndProtected(t *testing.T) {
	d, err := New(Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	removed := addGCJob(t, d, proto.StateExited, time.Now().Add(-time.Hour), true)
	removed.Spec.ChangeClientID = "0123456789abcdef0123456789abcdef"
	removed.result.Changes = &proto.ChangeSummary{Paths: []string{"artifact"}, PathCount: 1, BundleRoot: strings.Repeat("a", 64)}
	outcome, cleanupErr, err := d.removeJobReceipt(removed)
	if err != nil || cleanupErr != nil || outcome != jobRemovalRemoved {
		t.Fatalf("first removal = %v, cleanup=%v, err=%v", outcome, cleanupErr, err)
	}
	outcome, cleanupErr, err = d.removeJobReceipt(removed)
	if err != nil || cleanupErr != nil || outcome != jobRemovalSkipped {
		t.Fatalf("repeated removal = %v, cleanup=%v, err=%v", outcome, cleanupErr, err)
	}
	if outcome, raced := d.jobRemovalRace(removed); !raced || outcome != jobRemovalSkipped {
		t.Fatalf("removed receipt race = %v, raced=%t", outcome, raced)
	}
	if marker := d.collected[removed.ID]; !marker.ChangesPending || collectedMarkerExpired(marker, time.Now().Add(7*24*time.Hour)) {
		t.Fatalf("change collection marker = %+v, want durable pending-change marker", marker)
	}

	protected := addGCJob(t, d, proto.StateExited, time.Now().Add(-time.Hour), true)
	if !protected.acquireLogReader() {
		t.Fatal("could not acquire log reader")
	}
	defer protected.releaseLogReader()
	outcome, cleanupErr, err = d.removeJobReceipt(protected)
	if err != nil || cleanupErr != nil || outcome != jobRemovalProtected {
		t.Fatalf("protected removal = %v, cleanup=%v, err=%v", outcome, cleanupErr, err)
	}
	if outcome, raced := d.jobRemovalRace(protected); !raced || outcome != jobRemovalProtected {
		t.Fatalf("protected receipt race = %v, raced=%t", outcome, raced)
	}
}

func TestCleanupGCTombstonesRetriesWithoutFailingStartupPath(t *testing.T) {
	jobsDir := t.TempDir()
	tombstone := filepath.Join(jobsDir, ".gc-job")
	if err := os.Mkdir(tombstone, 0o700); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("busy")
	failures, err := cleanupGCTombstones(context.Background(), jobsDir, func(string) error { return wantErr })
	if err != nil || failures != 1 {
		t.Fatalf("failed tombstone cleanup = failures %d, err %v", failures, err)
	}
	if _, err := os.Stat(tombstone); err != nil {
		t.Fatalf("failed cleanup lost retryable tombstone: %v", err)
	}
	failures, err = cleanupGCTombstones(context.Background(), jobsDir, removeOwnedTree)
	if err != nil || failures != 0 {
		t.Fatalf("retried tombstone cleanup = failures %d, err %v", failures, err)
	}
	if _, err := os.Stat(tombstone); !os.IsNotExist(err) {
		t.Fatalf("retried tombstone remains: %v", err)
	}
}

func TestCleanupOwnedGCTombstonesOnlyTouchesCaller(t *testing.T) {
	d, err := New(Config{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	caller := Identity{UserID: 42, Login: "george@example.com"}
	other := Identity{UserID: 43, Login: "other@example.com"}
	callerID := proto.NewULID()
	otherID := proto.NewULID()
	for _, id := range []string{callerID, otherID} {
		if err := os.Mkdir(filepath.Join(d.jobsDir(), ".gc-"+id+"-"+proto.NewULID()), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	d.collected[callerID] = collectedRecord{Owner: caller.Owner(), CollectedAt: time.Now()}
	d.collected[otherID] = collectedRecord{Owner: other.Owner(), CollectedAt: time.Now()}

	var removed []string
	wantErr := errors.New("busy")
	failures, err := d.cleanupOwnedGCTombstones(context.Background(), caller, func(path string) error {
		removed = append(removed, path)
		return wantErr
	})
	if err != nil || failures != 1 || len(removed) != 1 || !strings.Contains(removed[0], callerID) {
		t.Fatalf("owned tombstone cleanup = failures %d, removed %v, err %v", failures, removed, err)
	}
}

func TestReplaceJSONDurableSyncsParentAfterRename(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "marker.json")
	wantErr := errors.New("directory sync failed")
	called := false
	err := replaceJSONDurableWith(dest, map[string]string{"state": "committed"}, func(dir string) error {
		called = true
		if dir != filepath.Dir(dest) {
			t.Fatalf("synced directory = %q, want %q", dir, filepath.Dir(dest))
		}
		if _, err := os.Stat(dest); err != nil {
			t.Fatalf("marker was not renamed before directory sync: %v", err)
		}
		return wantErr
	})
	if !called || !errors.Is(err, wantErr) {
		t.Fatalf("durable replace = called %t, err %v", called, err)
	}
}

func TestEnsureChildDirectoryDurableSyncsParentAfterCreate(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "collected")
	called := false
	err := ensureChildDirectoryDurableWith(child, 0o700, func(path string) error {
		called = true
		if path != parent {
			t.Fatalf("synced directory = %q, want %q", path, parent)
		}
		info, err := os.Stat(child)
		if err != nil || !info.IsDir() {
			t.Fatalf("child was not created before parent sync: info=%v err=%v", info, err)
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("durable child directory = called %t, err %v", called, err)
	}
}

func TestJobGCRejectsUnboundedOrUnknownPolicy(t *testing.T) {
	_, ts := testDaemon(t)
	for _, body := range []string{`{}`, `{"older_than_days":30}`} {
		resp, err := http.Post(ts.URL+"/v0/jobs/gc", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("job GC policy %s = %s, want 400", body, resp.Status)
		}
	}
}

func TestJobGCOnlyRemovesCallerOwnedReceipts(t *testing.T) {
	d, err := New(Config{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	own := addGCJob(t, d, proto.StateExited, time.Now().Add(-48*time.Hour), true)
	other := addGCJob(t, d, proto.StateExited, time.Now().Add(-48*time.Hour), true)
	own.Admission = proto.Admission{UserID: 42, UserLogin: "george@example.com"}
	other.Admission = proto.Admission{UserID: 43, UserLogin: "other@example.com"}
	keep := 0
	body, _ := json.Marshal(proto.JobGCRequest{Keep: &keep})
	request := httptest.NewRequest(http.MethodPost, "/v0/jobs/gc", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	d.handleJobGC(recorder, request, Identity{UserID: 42, Login: "george@example.com"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("job GC = %s", recorder.Result().Status)
	}
	if _, err := os.Stat(own.Dir); !os.IsNotExist(err) {
		t.Fatalf("caller's receipt still exists: %v", err)
	}
	if _, err := os.Stat(other.Dir); err != nil {
		t.Fatalf("other caller's receipt was removed: %v", err)
	}
}

func TestChangeReconciliationIsOwnerScopedAndPaginated(t *testing.T) {
	d, err := New(Config{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	identity := Identity{Login: "george@example.com", UserID: 42}
	owner := identity.Owner()
	clientID := "0123456789abcdef0123456789abcdef"
	want := make(map[string]bool, proto.ChangeReconciliationPageLimit+1)
	for range proto.ChangeReconciliationPageLimit + 1 {
		jobID := proto.NewULID()
		want[jobID] = true
		d.collected[jobID] = collectedRecord{
			Owner: owner, CollectedAt: time.Now(), ChangesPending: true, ChangeClientID: clientID,
		}
	}
	d.collected[proto.NewULID()] = collectedRecord{
		Owner: "other@example.com", CollectedAt: time.Now(), ChangesPending: true, ChangeClientID: clientID,
	}
	d.collected[proto.NewULID()] = collectedRecord{
		Owner: owner, CollectedAt: time.Now(), ChangesPending: true,
		ChangeClientID: "fedcba9876543210fedcba9876543210",
	}

	got := map[string]bool{}
	cursor := ""
	for {
		request := httptest.NewRequest(http.MethodGet,
			"/v0/change-reconciliation?client_id="+clientID+"&cursor="+cursor, nil)
		recorder := httptest.NewRecorder()
		d.handleChangeReconciliation(recorder, request, identity)
		if recorder.Code != http.StatusOK {
			t.Fatalf("collected jobs = %s", recorder.Result().Status)
		}
		var page proto.ChangeReconciliationPage
		if err := json.NewDecoder(recorder.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		if len(page.JobIDs) > proto.ChangeReconciliationPageLimit {
			t.Fatalf("collected page has %d IDs", len(page.JobIDs))
		}
		for _, jobID := range page.JobIDs {
			got[jobID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(got) != len(want) {
		t.Fatalf("collected jobs count = %d, want %d", len(got), len(want))
	}
	for jobID := range want {
		if !got[jobID] {
			t.Fatalf("collected jobs omitted %s", jobID)
		}
	}
}

func TestChangeReconciliationAcknowledgementRetiresExpiredChangeMarker(t *testing.T) {
	d, err := New(Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	clientID := "0123456789abcdef0123456789abcdef"
	jobID := proto.NewULID()
	record := collectedRecord{
		CollectedAt:    time.Now().Add(-collectedMarkerTTL - time.Minute),
		ChangesPending: true, ChangeClientID: clientID,
	}
	marker := filepath.Join(d.collectedDir(), jobID+".json")
	if err := replaceJSONDurable(marker, record); err != nil {
		t.Fatal(err)
	}
	d.collected[jobID] = record
	body, err := json.Marshal(proto.ChangeReconciliationAck{ClientID: clientID, JobIDs: []string{jobID}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v0/change-reconciliation/ack", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	d.handleChangeReconciliationAck(recorder, request, Identity{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("collection acknowledgement = %s: %s", recorder.Result().Status, recorder.Body.String())
	}
	var result proto.ChangeReconciliationAckResult
	if err := json.NewDecoder(recorder.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Acknowledged != 1 {
		t.Fatalf("acknowledged markers = %d, want 1", result.Acknowledged)
	}
	if _, ok := d.collected[jobID]; ok {
		t.Fatal("acknowledged expired marker remained in memory")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("acknowledged expired marker remains on disk: %v", err)
	}
}

func TestJobGCProtectsReceiptWithoutSettlementTime(t *testing.T) {
	d, ts := testDaemon(t)
	j := addGCJob(t, d, proto.StateExited, time.Now(), true)
	j.mu.Lock()
	j.result.SettledAt = nil
	j.mu.Unlock()
	seconds := int64((24 * time.Hour) / time.Second)
	result := postJobGC(t, ts.URL, proto.JobGCRequest{OlderThanSeconds: &seconds})
	if result.RemovedJobs != 0 || result.ProtectedJobs != 1 {
		t.Fatalf("receipt without settlement time GC = %+v", result)
	}
}

func TestTerminalResultsAlwaysRecordSettlementTime(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	id := proto.NewULID()
	resp := rawSubmit(t, ts.URL, id, root, []string{"/definitely/not/an/executable"})
	resp.Body.Close()
	status := waitTerminal(t, ts.URL, id)
	if status.Result == nil || status.Result.SettledAt == nil || status.Result.SettledAt.IsZero() {
		t.Fatalf("terminal result lacks settlement time: %+v", status.Result)
	}
}

func TestLoadExistingRemovesInterruptedGCTombstone(t *testing.T) {
	stateDir := t.TempDir()
	tombstone := filepath.Join(stateDir, "jobs", ".gc-old-job")
	if err := os.MkdirAll(tombstone, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tombstone, "io.log"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := os.Stat(tombstone); !os.IsNotExist(err) {
		t.Fatalf("GC tombstone remains after restart: %v", err)
	}
}

func TestCollectedJobIDCannotReplayAfterRestart(t *testing.T) {
	stateDir := t.TempDir()
	d, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(d.Handler())
	root := workspaceWith(t, nil)
	id := proto.NewULID()
	resp := rawSubmit(t, ts.URL, id, root, []string{"/bin/true"})
	resp.Body.Close()
	waitTerminal(t, ts.URL, id)
	keep := 0
	result := postJobGC(t, ts.URL, proto.JobGCRequest{Keep: &keep})
	if result.RemovedJobs != 1 {
		t.Fatalf("job GC = %+v", result)
	}
	marker, err := os.ReadFile(filepath.Join(d.collectedDir(), id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(marker, []byte("request_digest")) {
		t.Fatalf("collection marker retained a value-derived request digest: %s", marker)
	}
	ts.Close()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	d, err = New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ts = httptest.NewServer(d.Handler())
	defer ts.Close()
	resp = rawSubmit(t, ts.URL, id, root, []string{"/bin/true"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("reusing collected job id = %s, want 410 Gone", resp.Status)
	}
}

func TestLoadCollectedPrunesExpiredMarkers(t *testing.T) {
	stateDir := t.TempDir()
	collectedDir := filepath.Join(stateDir, "collected")
	if err := os.MkdirAll(collectedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := proto.NewULID()
	marker := filepath.Join(collectedDir, id+".json")
	if err := replaceJSON(marker, collectedRecord{
		Owner: "george@example.com", CollectedAt: time.Now().Add(-collectedMarkerTTL - time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	d, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, ok := d.collected[id]; ok {
		t.Fatal("expired collection marker was loaded")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("expired collection marker remains: %v", err)
	}
}

func TestLoadCollectedRetainsPendingChangeMarkersWithinAbandonmentBound(t *testing.T) {
	stateDir := t.TempDir()
	collectedDir := filepath.Join(stateDir, "collected")
	if err := os.MkdirAll(collectedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := proto.NewULID()
	marker := filepath.Join(collectedDir, id+".json")
	record := collectedRecord{
		Owner: "george@example.com", CollectedAt: time.Now().Add(-pendingChangeMarkerTTL + time.Minute), ChangesPending: true,
		ChangeClientID: "0123456789abcdef0123456789abcdef",
	}
	if err := replaceJSON(marker, record); err != nil {
		t.Fatal(err)
	}

	d, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if got, ok := d.collected[id]; !ok || !got.ChangesPending {
		t.Fatalf("old change collection marker = %+v, loaded=%t", got, ok)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("old change collection marker was removed: %v", err)
	}
}

func TestLoadCollectedPrunesAbandonedPendingChangeMarkers(t *testing.T) {
	stateDir := t.TempDir()
	collectedDir := filepath.Join(stateDir, "collected")
	if err := os.MkdirAll(collectedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := proto.NewULID()
	marker := filepath.Join(collectedDir, id+".json")
	record := collectedRecord{
		Owner: "george@example.com", CollectedAt: time.Now().Add(-pendingChangeMarkerTTL - time.Minute), ChangesPending: true,
		ChangeClientID: "0123456789abcdef0123456789abcdef",
	}
	if err := replaceJSON(marker, record); err != nil {
		t.Fatal(err)
	}

	d, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, ok := d.collected[id]; ok {
		t.Fatal("abandoned pending change marker was loaded")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("abandoned pending change marker remains: %v", err)
	}
}

func TestJobGCPrunesExpiredMarkersWithoutRestart(t *testing.T) {
	d, err := New(Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	id := proto.NewULID()
	record := collectedRecord{CollectedAt: time.Now().Add(-collectedMarkerTTL - time.Minute)}
	if err := replaceJSON(filepath.Join(d.collectedDir(), id+".json"), record); err != nil {
		t.Fatal(err)
	}
	d.collected[id] = record

	keep := 0
	body, _ := json.Marshal(proto.JobGCRequest{Keep: &keep})
	request := httptest.NewRequest(http.MethodPost, "/v0/jobs/gc", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	d.handleJobGC(recorder, request, Identity{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("job GC = %s", recorder.Result().Status)
	}
	if _, ok := d.collected[id]; ok {
		t.Fatal("expired collection marker remains loaded")
	}
	if _, err := os.Stat(filepath.Join(d.collectedDir(), id+".json")); !os.IsNotExist(err) {
		t.Fatalf("expired collection marker remains on disk: %v", err)
	}
}

func TestJobGCDryRunDoesNotAdvanceClockOrPruneExpiredMarkers(t *testing.T) {
	d, err := New(Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	id := proto.NewULID()
	record := collectedRecord{CollectedAt: time.Now().Add(-collectedMarkerTTL - time.Minute)}
	marker := filepath.Join(d.collectedDir(), id+".json")
	if err := replaceJSON(marker, record); err != nil {
		t.Fatal(err)
	}
	d.collected[id] = record
	clockBefore, err := os.ReadFile(d.admissionClockPath())
	if err != nil {
		t.Fatal(err)
	}

	keep := 0
	body, _ := json.Marshal(proto.JobGCRequest{Keep: &keep, DryRun: true})
	request := httptest.NewRequest(http.MethodPost, "/v0/jobs/gc", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	d.handleJobGC(recorder, request, Identity{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("job GC = %s", recorder.Result().Status)
	}
	clockAfter, err := os.ReadFile(d.admissionClockPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(clockBefore, clockAfter) {
		t.Fatal("dry-run advanced the durable admission clock")
	}
	if _, ok := d.collected[id]; !ok {
		t.Fatal("dry-run pruned the expired marker from memory")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("dry-run pruned the expired marker from disk: %v", err)
	}
}

func TestPruneCollectedRetainsExpiredMarkerUntilTombstoneIsGone(t *testing.T) {
	d, err := New(Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	id := proto.NewULID()
	record := collectedRecord{CollectedAt: time.Now().Add(-collectedMarkerTTL - time.Minute)}
	marker := filepath.Join(d.collectedDir(), id+".json")
	if err := replaceJSON(marker, record); err != nil {
		t.Fatal(err)
	}
	d.collected[id] = record
	tombstone := filepath.Join(d.jobsDir(), ".gc-"+id+"-"+proto.NewULID())
	if err := os.Mkdir(tombstone, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := d.pruneCollected(context.Background(), d.admissionNow(time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.collected[id]; !ok {
		t.Fatal("expired marker was pruned while its tombstone remained")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("retained marker is missing: %v", err)
	}

	if err := removeOwnedTree(tombstone); err != nil {
		t.Fatal(err)
	}
	if err := d.pruneCollected(context.Background(), d.admissionNow(time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.collected[id]; ok {
		t.Fatal("expired marker remained after tombstone cleanup")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("expired marker remains on disk: %v", err)
	}
}

func TestJobGCDoesNotHoldDaemonLockWhileInspectingJob(t *testing.T) {
	d, err := New(Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	j := addGCJob(t, d, proto.StateExited, time.Now().Add(-time.Hour), true)
	j.mu.Lock()
	jobLocked := true
	defer func() {
		if jobLocked {
			j.mu.Unlock()
		}
	}()

	keep := 0
	body, _ := json.Marshal(proto.JobGCRequest{Keep: &keep})
	request := httptest.NewRequest(http.MethodPost, "/v0/jobs/gc", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		d.handleJobGC(recorder, request, Identity{})
		close(done)
	}()

	// Give the handler time to reach the job lock held above. Other daemon
	// operations must still be able to acquire the global map lock.
	time.Sleep(25 * time.Millisecond)
	locked := make(chan struct{})
	go func() {
		d.mu.Lock()
		d.mu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("job GC held the daemon lock while waiting to inspect a job")
	}
	j.mu.Unlock()
	jobLocked = false
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("job GC did not finish after the job lock was released")
	}
}

func TestCanceledJobGCDoesNotDeleteReceipt(t *testing.T) {
	d, err := New(Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	j := addGCJob(t, d, proto.StateExited, time.Now().Add(-time.Hour), true)

	keep := 0
	body, _ := json.Marshal(proto.JobGCRequest{Keep: &keep})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, "/v0/jobs/gc", bytes.NewReader(body)).WithContext(ctx)
	d.handleJobGC(httptest.NewRecorder(), request, Identity{})

	if _, err := os.Stat(j.Dir); err != nil {
		t.Fatalf("canceled GC removed receipt: %v", err)
	}
}

func TestTreeBytesStopsWhenCanceled(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "data"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := treeBytes(ctx, root); err != context.Canceled {
		t.Fatalf("treeBytes error = %v, want context.Canceled", err)
	}
}

func TestSubmitRejectsJobIDOutsideAdmissionWindow(t *testing.T) {
	_, ts := testDaemon(t)
	for _, tt := range []struct {
		name string
		when time.Time
	}{
		{name: "stale", when: time.Now().Add(-jobIDMaxAge - time.Minute)},
		{name: "too far in future", when: time.Now().Add(jobIDMaxFutureSkew + time.Minute)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPut, ts.URL+"/v0/jobs/"+jobIDAt(tt.when), nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("submit with %s job ID = %s, want 400", tt.name, resp.Status)
			}
			if tt.name == "stale" && !bytes.Contains(body, []byte("too old")) {
				t.Fatalf("stale job ID response = %q", body)
			}
			if tt.name == "too far in future" && !bytes.Contains(body, []byte("future")) {
				t.Fatalf("future job ID response = %q", body)
			}
		})
	}
}

func TestKnownOldJobIDRemainsIdempotent(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	currentID := proto.NewULID()
	resp := rawSubmit(t, ts.URL, currentID, root, []string{"/bin/true"})
	resp.Body.Close()
	waitTerminal(t, ts.URL, currentID)

	oldID := jobIDAt(time.Now().Add(-jobIDMaxAge - time.Hour))
	d.mu.Lock()
	j := d.jobs[currentID]
	delete(d.jobs, currentID)
	j.ID = oldID
	d.jobs[oldID] = j
	d.mu.Unlock()

	retry := rawSubmit(t, ts.URL, oldID, root, []string{"/bin/true"})
	defer retry.Body.Close()
	if retry.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(retry.Body)
		t.Fatalf("known old job retry = %s, want 200: %s", retry.Status, body)
	}
}

func TestAdmissionClockDoesNotMoveBackwardAcrossRestart(t *testing.T) {
	stateDir := t.TempDir()
	d, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(26 * time.Hour)
	if _, err := d.advanceAdmissionClock(future); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	d, err = New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got := d.admissionNow(time.Now())
	if got.Before(future) {
		t.Fatalf("admission clock moved backward: got %v, want at least %v", got, future)
	}
	if err := validateNewJobID(jobIDAt(time.Now()), got); err == nil {
		t.Fatal("clock rollback made an ID from before the high-water mark admissible")
	}
}

func jobIDAt(when time.Time) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	ms := uint64(when.UnixMilli())
	prefix := make([]byte, 10)
	for i := len(prefix) - 1; i >= 0; i-- {
		prefix[i] = alphabet[ms&31]
		ms >>= 5
	}
	return string(prefix) + proto.NewULID()[10:]
}
