package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

const testChangeClientID = "0123456789abcdef0123456789abcdef"

func TestTerminateCancelsReapedChangeCollection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{
		ID: proto.NewULID(), Dir: t.TempDir(), state: proto.StateRunning, reaped: true,
		changeCancel: cancel,
	}
	d := &Daemon{}
	if handled, err := d.cancelBeforeStart(context.Background(), j, "user-kill", syscall.SIGKILL); handled || err != nil {
		t.Fatalf("cancelBeforeStart() = handled %t, error %v", handled, err)
	}
	if err := j.terminate("user-kill", syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("terminate did not cancel change collection")
	}
}

func TestRuntimeDeadlineDoesNotCancelReapedChangeCollection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j := &Job{
		ID: proto.NewULID(), Dir: t.TempDir(), state: proto.StateRunning, reaped: true,
		changeCancel: cancel,
	}
	if err := j.terminate("runtime", syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		t.Fatal("expired process timer canceled post-process change collection")
	default:
	}
}

func TestRuntimeDeadlineDoesNotQueueChangeCancellationDuringExitHandoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j := &Job{ID: proto.NewULID(), Dir: t.TempDir(), state: proto.StateRunning}
	if handled := j.requestChangeCollectionCancellation("runtime"); handled {
		t.Fatal("runtime deadline queued post-process change cancellation")
	}
	j.transitionAfterProcessExit(cancel)
	select {
	case <-ctx.Done():
		t.Fatal("queued runtime deadline canceled post-process change collection")
	default:
	}
}

func TestChangeCollectionGetsDedicatedPostProcessDeadline(t *testing.T) {
	finishedAt := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	ctx, cancel := changeCollectionContext(finishedAt)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.Equal(finishedAt.Add(changeCollectionTimeout)) {
		t.Fatalf("collection deadline = %v, %v", deadline, ok)
	}
}

func TestPostCommitCollectionErrorKeepsCommittedBundle(t *testing.T) {
	jobDir := t.TempDir()
	bundleDir := filepath.Join(jobDir, changeops.BundleDirectory)
	if err := os.Mkdir(bundleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	collected, err := settleChangeCollection(jobDir, true, errors.New("restoring workspace permissions"), nil)
	if err != nil || !collected {
		t.Fatalf("settleChangeCollection() = collected %t, error %v", collected, err)
	}
	if _, statErr := os.Lstat(bundleDir); statErr != nil {
		t.Fatalf("committed collection bundle was removed: %v", statErr)
	}
}

func TestSignalCancelsReapedChangeCollection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{
		ID: proto.NewULID(), Dir: t.TempDir(), state: proto.StateRunning, reaped: true,
		changeCancel: cancel,
	}
	if err := j.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("signal did not cancel change collection")
	}
}

func TestProcessExitHandoffHonorsPendingChangeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{ID: proto.NewULID(), Dir: t.TempDir(), state: proto.StateRunning}
	if handled := j.requestChangeCollectionCancellation("interrupt"); !handled {
		t.Fatal("change cancellation was not recorded before the process-exit handoff")
	}
	j.transitionAfterProcessExit(cancel)
	if !j.reaped || j.cmd != nil {
		t.Fatalf("process exit transition = reaped %t, cmd %v", j.reaped, j.cmd)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("pending change cancellation was not honored")
	}
}

func TestPreStartCancellationRejectsReapedJob(t *testing.T) {
	j := &Job{ID: proto.NewULID(), Dir: t.TempDir(), state: proto.StateRunning, reaped: true}
	d := &Daemon{}
	handled, err := d.cancelBeforeStart(context.Background(), j, "user-signal", syscall.SIGINT)
	if err == nil || !handled || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("cancelBeforeStart() = handled %t, error %v; want terminal rejection", handled, err)
	}
}

type deadlineResponseWriter struct {
	header    http.Header
	deadlines []time.Time
	flushes   int
}

func (w *deadlineResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (*deadlineResponseWriter) WriteHeader(int)             {}
func (*deadlineResponseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *deadlineResponseWriter) SetWriteDeadline(v time.Time) error {
	w.deadlines = append(w.deadlines, v)
	return nil
}
func (w *deadlineResponseWriter) FlushError() error { w.flushes++; return nil }

func TestIdleDeadlineWriterRefreshesAndClearsDeadline(t *testing.T) {
	destination := &deadlineResponseWriter{}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	w := newIdleDeadlineWriter(destination, time.Minute)
	w.now = func() time.Time { return now }
	_, _ = w.Write([]byte("first"))
	now = now.Add(30 * time.Second)
	_, _ = w.Write([]byte("second"))
	if err := w.flush(); err != nil {
		t.Fatal(err)
	}
	w.clear()
	if len(destination.deadlines) != 4 || destination.flushes != 1 || !destination.deadlines[3].IsZero() {
		t.Fatalf("deadlines = %v, flushes = %d", destination.deadlines, destination.flushes)
	}
}

func submitChangeJob(t *testing.T, d *Daemon, url, root string, argv []string) (string, proto.JobStatus) {
	t.Helper()
	paths, _, selection, err := snapshot.SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshot.Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	id := proto.NewULID()
	resp := rawSubmitSpec(t, url, id, root, proto.Spec{
		V: proto.ProtoVersion, Argv: argv, ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits(),
		ChangeClientID: testChangeClientID, Selection: selection,
	}, manifest)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("submit = %s: %s", resp.Status, body)
	}
	resp.Body.Close()
	return id, waitTerminal(t, url, id)
}

func TestWorkspaceChangesAreDurableAndDownloadable(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	id, status := submitChangeJob(t, d, ts.URL, root,
		[]string{"/bin/sh", "-c", "mkdir -p dist && printf artifact > dist/result.txt"})
	if status.Result == nil || !status.Result.ChangesOK || status.Result.Changes == nil {
		t.Fatalf("result = %+v", status.Result)
	}
	bundle, err := changeops.Load(filepath.Join(d.jobsDir(), id))
	if err != nil {
		t.Fatal(err)
	}
	if bundle.RootHash() != status.Result.Changes.BundleRoot || fmt.Sprint(bundle.Paths) != "[dist]" || bundle.Bytes != 8 {
		t.Fatalf("bundle = %+v; summary = %+v", bundle, status.Result.Changes)
	}
	if _, err := os.Lstat(filepath.Join(d.jobsDir(), id, "workspace")); !os.IsNotExist(err) {
		t.Fatalf("workspace retained after collection: %v", err)
	}
	d.mu.Lock()
	job := d.jobs[id]
	d.mu.Unlock()
	job.mu.Lock()
	retainedBaselineEntries := len(job.baseline.Entries)
	job.mu.Unlock()
	if retainedBaselineEntries != 0 {
		t.Fatalf("terminal job retained %d baseline manifest entries", retainedBaselineEntries)
	}

	resp, err := http.Get(ts.URL + "/v0/jobs/" + id + "/changes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK || err != nil || mediaType != "multipart/form-data" {
		t.Fatalf("download = %s, %q, %v", resp.Status, mediaType, err)
	}
	mr := multipart.NewReader(resp.Body, params["boundary"])
	part, _ := mr.NextPart()
	var downloaded proto.ChangeBundle
	if err := json.NewDecoder(part).Decode(&downloaded); err != nil {
		t.Fatal(err)
	}
	staged := t.TempDir()
	base := filepath.Join(staged, "base")
	remote := filepath.Join(staged, "remote")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(remote, 0o700); err != nil {
		t.Fatal(err)
	}
	part, _ = mr.NextPart()
	if err := changeops.ExtractBase(part, base, downloaded, proto.DefaultLimits().MaxChangeBytes); err != nil {
		t.Fatal(err)
	}
	part, _ = mr.NextPart()
	if err := changeops.ExtractRemote(part, remote, downloaded, proto.DefaultLimits().MaxChangeBytes); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(remote, "dist", "result.txt"))
	if err != nil || string(got) != "artifact" {
		t.Fatalf("downloaded artifact = %q, %v", got, err)
	}
}

func TestFailedCommandStillRetainsWorkspaceChanges(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	_, status := submitChangeJob(t, d, ts.URL, root,
		[]string{"/bin/sh", "-c", "printf partial > report.txt; exit 7"})
	if status.Result == nil || status.Result.ExitCode == nil || *status.Result.ExitCode != 7 ||
		!status.Result.ChangesOK || status.Result.Changes == nil {
		t.Fatalf("result = %+v", status.Result)
	}
}

func TestUnchangedJobHasNoWorkspaceChangeBundle(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"input": "stable"})
	id, status := submitChangeJob(t, d, ts.URL, root, []string{"/bin/true"})
	if status.Result == nil || !status.Result.ChangesOK || status.Result.Changes != nil {
		t.Fatalf("result = %+v", status.Result)
	}
	if _, err := changeops.Load(filepath.Join(d.jobsDir(), id)); !os.IsNotExist(err) {
		t.Fatalf("unchanged job bundle error = %v", err)
	}
}

func TestRunReportsRetainedChangesUntilExplicitFetch(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"report.txt": "old"})
	var stderr bytes.Buffer
	code := client.Run(client.RunOptions{
		PeerURL: ts.URL, Root: root, Argv: []string{"/bin/sh", "-c", "printf fetched > report.txt"},
		Stdout: io.Discard, Stderr: &stderr,
	})
	if code != 0 {
		t.Fatalf("run exit = %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "workspace changes retained") ||
		!strings.Contains(stderr.String(), "errand fetch") {
		t.Fatalf("run did not explain retained changes: %s", stderr.String())
	}
	got, _ := os.ReadFile(filepath.Join(root, "report.txt"))
	if string(got) != "old" {
		t.Fatalf("workspace changed before apply: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(stateHome, "errand", "downloads")); !os.IsNotExist(err) {
		t.Fatalf("foreground run downloaded retained changes: %v", err)
	}
	id := lastJobID(t, d)
	if _, err := client.FetchChanges(client.ChangeFetchOptions{PeerURL: ts.URL, JobID: id, Apply: true, CallerDir: root}); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(filepath.Join(root, "report.txt"))
	if string(got) != "fetched" {
		t.Fatalf("applied value = %q", got)
	}
}

func TestRunAppliesRetainedChangesOnSuccessWhenRequested(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	_, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"report.txt": "old"})
	var stderr bytes.Buffer
	code := client.Run(client.RunOptions{
		PeerURL: ts.URL, Root: root, Argv: []string{"/bin/sh", "-c", "printf applied > report.txt"},
		ApplyOnSuccess: true, Stdout: io.Discard, Stderr: &stderr,
	})
	if code != 0 {
		t.Fatalf("run exit = %d; stderr: %s", code, stderr.String())
	}
	got, err := os.ReadFile(filepath.Join(root, "report.txt"))
	if err != nil || string(got) != "applied" {
		t.Fatalf("automatically applied value = %q, %v", got, err)
	}
	if !strings.Contains(stderr.String(), "workspace changes applied") {
		t.Fatalf("automatic apply diagnostic = %q", stderr.String())
	}
}

func TestRunDoesNotApplyChangesFromFailedCommand(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	_, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"report.txt": "old"})
	code := client.Run(client.RunOptions{
		PeerURL: ts.URL, Root: root,
		Argv:           []string{"/bin/sh", "-c", "printf remote > report.txt; exit 7"},
		ApplyOnSuccess: true, Stdout: io.Discard, Stderr: io.Discard,
	})
	if code != 7 {
		t.Fatalf("run exit = %d, want 7", code)
	}
	got, err := os.ReadFile(filepath.Join(root, "report.txt"))
	if err != nil || string(got) != "old" {
		t.Fatalf("failed command changed local workspace = %q, %v", got, err)
	}
}

func TestFetchApplyConflictsMaterializesTextConflict(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"report.txt": "base\n"})
	if code := client.Run(client.RunOptions{
		PeerURL: ts.URL, Root: root,
		Argv:   []string{"/bin/sh", "-c", "printf 'remote\\n' > report.txt"},
		Stdout: io.Discard, Stderr: io.Discard,
	}); code != 0 {
		t.Fatalf("run exit = %d", code)
	}
	if err := os.WriteFile(filepath.Join(root, "report.txt"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := lastJobID(t, d)
	staged, err := client.FetchChanges(client.ChangeFetchOptions{
		PeerURL: ts.URL, JobID: id, Apply: true, MaterializeConflicts: true, CallerDir: root,
	})
	var conflict *changeops.MergeConflictError
	if !errors.As(err, &conflict) || staged == "" || fmt.Sprint(conflict.Paths) != "[report.txt]" {
		t.Fatalf("materialized fetch = staged %q, error %v", staged, err)
	}
	got, readErr := os.ReadFile(filepath.Join(root, "report.txt"))
	if readErr != nil || !bytes.Contains(got, []byte("<<<<<<< local")) ||
		!bytes.Contains(got, []byte("local")) || !bytes.Contains(got, []byte("remote")) {
		t.Fatalf("materialized conflict = %q, %v", got, readErr)
	}
}

func TestFetchApplyCanSelectOneChangedPath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"first.txt": "old-first", "second.txt": "old-second"})
	if code := client.Run(client.RunOptions{
		PeerURL: ts.URL, Root: root,
		Argv:   []string{"/bin/sh", "-c", "printf new-first > first.txt; printf new-second > second.txt"},
		Stdout: io.Discard, Stderr: io.Discard,
	}); code != 0 {
		t.Fatalf("run exit = %d", code)
	}
	id := lastJobID(t, d)
	if _, err := client.FetchChanges(client.ChangeFetchOptions{
		PeerURL: ts.URL, JobID: id, Apply: true, Path: "second.txt", CallerDir: root,
	}); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(root, "first.txt"))
	second, _ := os.ReadFile(filepath.Join(root, "second.txt"))
	if string(first) != "old-first" || string(second) != "new-second" {
		t.Fatalf("selected apply = %q, %q", first, second)
	}
	if _, err := client.FetchChanges(client.ChangeFetchOptions{PeerURL: ts.URL, JobID: id, Path: "unchanged"}); err == nil || !strings.Contains(err.Error(), "was not changed") {
		t.Fatalf("unchanged selector error = %v", err)
	}
}

func TestRestartMarksUnfinishedChangeCollectionIncomplete(t *testing.T) {
	stateDir := t.TempDir()
	id := proto.NewULID()
	dir := filepath.Join(stateDir, "jobs", id)
	if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeJobFixture(t, dir, proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/bin/true"}, Limits: proto.DefaultLimits(), ChangeClientID: testChangeClientID,
	})
	d, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.mu.Lock()
	j := d.jobs[id]
	d.mu.Unlock()
	if j == nil || j.result == nil || j.result.ChangesOK || j.result.State != proto.StateAmbiguous {
		t.Fatalf("reconciled result = %+v", j)
	}
}

func TestRestartPublishesBundleCommittedBeforeResult(t *testing.T) {
	stateDir := t.TempDir()
	id := proto.NewULID()
	dir := filepath.Join(stateDir, "jobs", id)
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "result.txt"), []byte("durable"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := changeops.CollectWorkspaceChangesContext(context.Background(), workspace, dir, proto.Manifest{}, proto.SelectionPolicy{}, 1<<20)
	if err != nil || !collected {
		t.Fatalf("collect = %+v, %t, %v", bundle, collected, err)
	}
	writeJobFixture(t, dir, proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/bin/true"}, Limits: proto.DefaultLimits(), ChangeClientID: testChangeClientID,
	})
	d, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.mu.Lock()
	j := d.jobs[id]
	d.mu.Unlock()
	if j == nil || j.result == nil || !j.result.ChangesOK || j.result.Changes == nil ||
		j.result.Changes.BundleRoot != bundle.RootHash() || j.result.State != proto.StateAmbiguous {
		t.Fatalf("reconciled result = %+v", j)
	}
}

func writeJobFixture(t *testing.T, dir string, spec proto.Spec) {
	t.Helper()
	for name, value := range map[string]any{
		"spec.json":      proto.NewReceiptSpec(spec),
		"admission.json": proto.Admission{Method: "insecure-test", Time: time.Now()},
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestValidateSpecRequiresClientIdentityAndBoundedChanges(t *testing.T) {
	manifest := proto.Manifest{}
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/bin/true"}, ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits(),
	}
	if err := validateSpec(spec, proto.DefaultLimits()); err == nil || !strings.Contains(err.Error(), "change_client_id") {
		t.Fatalf("missing client identity validation = %v", err)
	}
	spec.ChangeClientID = testChangeClientID
	if err := validateSpec(spec, proto.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	spec.Selection.Ignore = []string{"first\nsecond"}
	if err := validateSpec(spec, proto.DefaultLimits()); err == nil || !strings.Contains(err.Error(), "selection policy") {
		t.Fatalf("invalid selection policy validation = %v", err)
	}
	spec.Selection = proto.SelectionPolicy{}
	spec.NoSnapshot = true
	spec.Selection.Ignore = []string{"generated/"}
	if err := validateSpec(spec, proto.DefaultLimits()); err == nil || !strings.Contains(err.Error(), "empty selection policy") {
		t.Fatalf("no-snapshot selection policy validation = %v", err)
	}
	spec.Selection = proto.SelectionPolicy{CaseFold: true}
	if err := validateSpec(spec, proto.DefaultLimits()); err == nil || !strings.Contains(err.Error(), "empty selection policy") {
		t.Fatalf("no-snapshot case-fold policy validation = %v", err)
	}
	spec.NoSnapshot = false
	spec.Selection = proto.SelectionPolicy{}
	spec.Limits.MaxChangeBytes++
	if err := validateSpec(spec, proto.DefaultLimits()); err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("oversized change limit validation = %v", err)
	}
	spec.Limits = proto.DefaultLimits()
	spec.Argv = []string{strings.Repeat("x", maxReceiptSpecBytes)}
	if err := validateSpec(spec, proto.DefaultLimits()); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("oversized receipt metadata validation = %v", err)
	}
}

func TestSummarizeChangeBundleBoundsPathPreview(t *testing.T) {
	paths := make([]string, maxChangePreviewPaths+20)
	for i := range paths {
		paths[i] = fmt.Sprintf("artifact-%04d", i)
	}
	bundle := proto.ChangeBundle{V: changeops.BundleVersion, Paths: paths, Bytes: 42}
	summary := summarizeChangeBundle(bundle)
	if summary.PathCount != len(paths) || !summary.PathsTruncated || len(summary.Paths) != maxChangePreviewPaths {
		t.Fatalf("bounded change summary = %+v", summary)
	}
	if summary.Paths[0] != paths[0] || summary.Paths[len(summary.Paths)-1] != paths[maxChangePreviewPaths-1] {
		t.Fatalf("change summary preview = %v", summary.Paths)
	}
}

func TestChangeByteLimitIsRecorded(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	paths, _, _, _ := snapshot.SelectFiles(root)
	manifest, _ := snapshot.Build(root, paths)
	limits := proto.DefaultLimits()
	limits.MaxChangeBytes = 3
	id := proto.NewULID()
	resp := rawSubmitSpec(t, ts.URL, id, root, proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/bin/sh", "-c", "printf large > result.txt"},
		ManifestRoot: manifest.RootHash(), Limits: limits, ChangeClientID: testChangeClientID,
	}, manifest)
	resp.Body.Close()
	status := waitTerminal(t, ts.URL, id)
	if status.Result == nil || status.Result.ChangesOK || status.Result.LimitExceeded != "change_bytes" {
		t.Fatalf("change limit result = %+v", status.Result)
	}
}

func TestChangeCollectionLimitClassification(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{err: changeops.ErrEntryLimitExceeded, want: "change_entries"},
		{err: changeops.ErrByteLimitExceeded, want: "change_bytes"},
		{err: changeops.ErrLimitExceeded, want: "change_bytes"},
		{err: fmt.Errorf("packing changes: %w", context.DeadlineExceeded), want: "change_deadline"},
		{err: context.Canceled, want: ""},
	} {
		if got := changeCollectionLimit(test.err); got != test.want {
			t.Fatalf("changeCollectionLimit(%v) = %q, want %q", test.err, got, test.want)
		}
	}
}
