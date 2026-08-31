package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	outputops "github.com/lydakis/errand/internal/outputs"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

const testOutputClientID = "0123456789abcdef0123456789abcdef"

func TestTerminateCancelsPostProcessOutputCollection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{
		ID: proto.NewULID(), Dir: t.TempDir(), state: proto.StateRunning, reaped: true,
		outputCancel: cancel,
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
		t.Fatal("terminate did not cancel output collection")
	}
}

func TestOutputCollectionDeadlineIncludesProcessRuntime(t *testing.T) {
	startedAt := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	ctx, cancel := outputCollectionContext(startedAt, 10)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("output collection context has no deadline")
	}
	want := startedAt.Add(10 * time.Second)
	if !deadline.Equal(want) {
		t.Fatalf("output collection deadline = %v, want %v", deadline, want)
	}
}

func TestProcessExitHandoffHonorsPendingOutputCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{
		ID: proto.NewULID(), Dir: t.TempDir(), state: proto.StateRunning,
		Spec: proto.Spec{Outputs: []proto.OutputSpec{{Path: "artifact"}}},
	}
	if handled := j.requestOutputCollectionCancellation("interrupt"); !handled {
		t.Fatal("output cancellation was not recorded before the process-exit handoff")
	}
	j.transitionAfterProcessExit(cancel)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("output collection context was not canceled")
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

func (*deadlineResponseWriter) WriteHeader(int) {}

func (*deadlineResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

func (w *deadlineResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func (w *deadlineResponseWriter) FlushError() error {
	w.flushes++
	return nil
}

func TestIdleDeadlineWriterRefreshesAndClearsDeadline(t *testing.T) {
	destination := &deadlineResponseWriter{}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	w := newIdleDeadlineWriter(destination, time.Minute)
	w.now = func() time.Time { return now }
	if _, err := w.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	if _, err := w.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}
	if err := w.flush(); err != nil {
		t.Fatal(err)
	}
	w.clear()
	if len(destination.deadlines) != 4 {
		t.Fatalf("deadlines = %v", destination.deadlines)
	}
	if want := time.Date(2026, time.August, 30, 12, 1, 0, 0, time.UTC); !destination.deadlines[0].Equal(want) {
		t.Fatalf("first deadline = %v, want %v", destination.deadlines[0], want)
	}
	if want := time.Date(2026, time.August, 30, 12, 1, 30, 0, time.UTC); !destination.deadlines[1].Equal(want) {
		t.Fatalf("second deadline = %v, want %v", destination.deadlines[1], want)
	}
	if !destination.deadlines[2].Equal(destination.deadlines[1]) {
		t.Fatalf("flush deadline = %v, want %v", destination.deadlines[2], destination.deadlines[1])
	}
	if destination.flushes != 1 {
		t.Fatalf("flushes = %d, want 1", destination.flushes)
	}
	if !destination.deadlines[3].IsZero() {
		t.Fatalf("cleared deadline = %v", destination.deadlines[3])
	}
}

func submitOutputJob(t *testing.T, d *Daemon, url, root string, argv []string, outputs []proto.OutputSpec) (string, proto.JobStatus) {
	t.Helper()
	paths, _, err := snapshot.SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshot.Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := outputops.NormalizeSpecs(outputs)
	if err != nil {
		t.Fatal(err)
	}
	id := proto.NewULID()
	resp := rawSubmitSpec(t, url, id, root, proto.Spec{
		V: proto.ProtoVersion, Argv: argv, ManifestRoot: manifest.RootHash(),
		Limits: proto.DefaultLimits(), Outputs: normalized, OutputClientID: testOutputClientID,
	}, manifest)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("submit = %s: %s", resp.Status, body)
	}
	resp.Body.Close()
	return id, waitTerminal(t, url, id)
}

func TestDeclaredOutputCollectionIsDurableAndDownloadable(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	id, status := submitOutputJob(t, d, ts.URL, root,
		[]string{"/bin/sh", "-c", "mkdir -p dist && printf artifact > dist/result.txt"},
		[]proto.OutputSpec{{Path: "dist", Apply: proto.OutputApplyManual}},
	)
	if status.Result == nil || !status.Result.OutputsOK || status.Result.Outputs == nil {
		t.Fatalf("output result = %+v", status.Result)
	}
	if _, err := os.Lstat(filepath.Join(d.jobsDir(), id, "workspace")); !os.IsNotExist(err) {
		t.Fatalf("workspace retained after collection: %v", err)
	}
	bundle, err := outputops.Load(filepath.Join(d.jobsDir(), id))
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Manifest.RootHash() != status.Result.Outputs.ManifestRoot || bundle.Bytes != 8 {
		t.Fatalf("bundle = %+v; summary = %+v", bundle, status.Result.Outputs)
	}

	resp, err := http.Get(ts.URL + "/v0/jobs/" + id + "/outputs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK || err != nil || mediaType != "multipart/form-data" {
		t.Fatalf("output download = %s, %q, %v", resp.Status, mediaType, err)
	}
	mr := multipart.NewReader(resp.Body, params["boundary"])
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "bundle" {
		t.Fatalf("bundle part = %v, %v", part, err)
	}
	var downloaded proto.OutputBundle
	if err := json.NewDecoder(part).Decode(&downloaded); err != nil {
		t.Fatal(err)
	}
	part, err = mr.NextPart()
	if err != nil || part.FormName() != "archive" {
		t.Fatalf("archive part = %v, %v", part, err)
	}
	dest := t.TempDir()
	if err := outputops.Extract(part, dest, downloaded, proto.DefaultLimits().MaxOutputBytes); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "dist", "result.txt"))
	if err != nil || string(got) != "artifact" {
		t.Fatalf("downloaded artifact = %q, %v", got, err)
	}
}

func TestOutputCollectionConditionsAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		command    string
		collect    string
		wantBundle bool
		wantOK     bool
		wantExit   int
		wantError  string
	}{
		{name: "success mode skips failed process", command: "printf partial > report.txt; exit 7", collect: proto.OutputCollectSuccess, wantOK: true, wantExit: 7},
		{name: "always mode collects failed process", command: "printf partial > report.txt; exit 7", collect: proto.OutputCollectAlways, wantBundle: true, wantOK: true, wantExit: 7},
		{name: "missing declared output fails transaction", command: "exit 0", collect: proto.OutputCollectAlways, wantOK: false, wantExit: 0, wantError: "does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, ts := testDaemon(t)
			root := workspaceWith(t, nil)
			id, status := submitOutputJob(t, d, ts.URL, root,
				[]string{"/bin/sh", "-c", tc.command},
				[]proto.OutputSpec{{Path: "report.txt", Collect: tc.collect, Apply: proto.OutputApplyManual}},
			)
			if status.Result == nil || status.Result.OutputsOK != tc.wantOK {
				t.Fatalf("result = %+v", status.Result)
			}
			if status.Result.ExitCode == nil || *status.Result.ExitCode != tc.wantExit {
				t.Fatalf("exit result = %+v", status.Result)
			}
			if (status.Result.Outputs != nil) != tc.wantBundle {
				t.Fatalf("output summary = %+v, want bundle %v", status.Result.Outputs, tc.wantBundle)
			}
			if tc.wantError != "" && !strings.Contains(status.Result.TransactionError, tc.wantError) {
				t.Fatalf("transaction error = %q", status.Result.TransactionError)
			}
			_, err := outputops.Load(filepath.Join(d.jobsDir(), id))
			if tc.wantBundle && err != nil {
				t.Fatal(err)
			}
			if !tc.wantBundle && !os.IsNotExist(err) {
				t.Fatalf("unexpected bundle load error: %v", err)
			}
		})
	}
}

func TestRunAutoAppliesOutputs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	_, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"dist/result.txt": "old"})
	var stderr bytes.Buffer
	code := client.Run(client.RunOptions{
		PeerURL: ts.URL, Root: root,
		Argv:    []string{"/bin/sh", "-c", "printf new > dist/result.txt"},
		Outputs: []proto.OutputSpec{{Path: "dist", Collect: proto.OutputCollectSuccess, Apply: proto.OutputApplyAuto}},
		Stdout:  io.Discard, Stderr: &stderr,
	})
	if code != 0 {
		t.Fatalf("run exit = %d; stderr: %s", code, stderr.String())
	}
	got, err := os.ReadFile(filepath.Join(root, "dist", "result.txt"))
	if err != nil || string(got) != "new" {
		t.Fatalf("applied output = %q, %v", got, err)
	}
	if !strings.Contains(stderr.String(), "applied output dist") || !strings.Contains(stderr.String(), "outputs staged at") {
		t.Fatalf("output diagnostics = %q", stderr.String())
	}
}

func TestRunRefusesToOverwriteConcurrentLocalChange(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"result.txt": "old"})
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- client.Run(client.RunOptions{
			PeerURL: ts.URL, Root: root,
			Argv:    []string{"/bin/sh", "-c", "sleep 1; printf remote > result.txt"},
			Outputs: []proto.OutputSpec{{Path: "result.txt", Collect: proto.OutputCollectSuccess, Apply: proto.OutputApplyAuto}},
			Stdout:  io.Discard, Stderr: &stderr,
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		running := len(d.running) > 0
		d.mu.Unlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(root, "result.txt"), []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != client.ExitTransaction {
		t.Fatalf("run exit = %d; stderr: %s", code, stderr.String())
	}
	got, err := os.ReadFile(filepath.Join(root, "result.txt"))
	if err != nil || string(got) != "local" {
		t.Fatalf("conflicting local output = %q, %v", got, err)
	}
	if !strings.Contains(stderr.String(), "conflicts with local changes") {
		t.Fatalf("conflict diagnostic = %q", stderr.String())
	}
}

func TestFetchApplyUsesOriginalManualBaseline(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"report.txt": "old"})
	var stderr bytes.Buffer
	code := client.Run(client.RunOptions{
		PeerURL: ts.URL, Root: root,
		Argv:    []string{"/bin/sh", "-c", "printf fetched > report.txt"},
		Outputs: []proto.OutputSpec{{Path: "report.txt", Collect: proto.OutputCollectSuccess, Apply: proto.OutputApplyManual}},
		Stdout:  io.Discard, Stderr: &stderr,
	})
	if code != 0 {
		t.Fatalf("run exit = %d; stderr: %s", code, stderr.String())
	}
	got, err := os.ReadFile(filepath.Join(root, "report.txt"))
	if err != nil || string(got) != "old" {
		t.Fatalf("manual output applied early = %q, %v", got, err)
	}
	id := lastJobID(t, d)
	staged, err := client.FetchOutputs(client.OutputFetchOptions{PeerURL: ts.URL, JobID: id, Apply: true, CallerDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if staged == "" {
		t.Fatal("fetch returned no staging path")
	}
	got, err = os.ReadFile(filepath.Join(root, "report.txt"))
	if err != nil || string(got) != "fetched" {
		t.Fatalf("manual fetched output = %q, %v", got, err)
	}
}

func TestFetchApplyCanSelectOneDeclaredOutput(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"first.txt": "old-first", "second.txt": "old-second"})
	code := client.Run(client.RunOptions{
		PeerURL: ts.URL, Root: root,
		Argv: []string{"/bin/sh", "-c", "printf new-first > first.txt; printf new-second > second.txt"},
		Outputs: []proto.OutputSpec{
			{Path: "first.txt", Collect: proto.OutputCollectSuccess, Apply: proto.OutputApplyManual},
			{Path: "second.txt", Collect: proto.OutputCollectSuccess, Apply: proto.OutputApplyManual},
		},
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if code != 0 {
		t.Fatalf("run exit = %d", code)
	}
	id := lastJobID(t, d)
	selected, err := client.FetchOutputs(client.OutputFetchOptions{
		PeerURL: ts.URL, JobID: id, Apply: true, OutputPath: "second.txt", CallerDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(selected) != "second.txt" {
		t.Fatalf("selected staging path = %q", selected)
	}
	first, err := os.ReadFile(filepath.Join(root, "first.txt"))
	if err != nil || string(first) != "old-first" {
		t.Fatalf("unselected output = %q, %v", first, err)
	}
	second, err := os.ReadFile(filepath.Join(root, "second.txt"))
	if err != nil || string(second) != "new-second" {
		t.Fatalf("selected output = %q, %v", second, err)
	}
	if _, err := client.FetchOutputs(client.OutputFetchOptions{
		PeerURL: ts.URL, JobID: id, OutputPath: "undeclared.txt",
	}); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("undeclared selector error = %v", err)
	}
}

func TestOutputBundleSurvivesDaemonRestart(t *testing.T) {
	stateDir := t.TempDir()
	d1, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	ts1 := httptest.NewServer(d1.Handler())
	root := workspaceWith(t, nil)
	id, status := submitOutputJob(t, d1, ts1.URL, root,
		[]string{"/bin/sh", "-c", "printf durable > result.txt"},
		[]proto.OutputSpec{{Path: "result.txt", Collect: proto.OutputCollectSuccess, Apply: proto.OutputApplyManual}},
	)
	if status.Result == nil || status.Result.Outputs == nil {
		t.Fatalf("result = %+v", status.Result)
	}
	ts1.Close()
	if err := d1.Close(); err != nil {
		t.Fatal(err)
	}

	d2, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	ts2 := httptest.NewServer(d2.Handler())
	defer ts2.Close()
	resp, err := http.Get(ts2.URL + "/v0/jobs/" + id + "/outputs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("post-restart output download = %s: %s", resp.Status, body)
	}
}

func TestJobGCProtectsActiveOutputDownload(t *testing.T) {
	d, _ := testDaemon(t)
	j := addGCJob(t, d, proto.StateExited, time.Now().Add(-48*time.Hour), true)
	if !j.acquireOutputReader() {
		t.Fatal("could not acquire output reader")
	}
	j.mu.Lock()
	_, eligible := gcEligibleLocked(j)
	j.mu.Unlock()
	if eligible {
		t.Fatal("job with active output reader was GC-eligible")
	}
	j.releaseOutputReader()
	j.mu.Lock()
	_, eligible = gcEligibleLocked(j)
	j.mu.Unlock()
	if !eligible {
		t.Fatal("settled job was not GC-eligible after output reader release")
	}
}

func TestRestartMarksUnfinishedDeclaredOutputsIncomplete(t *testing.T) {
	stateDir := t.TempDir()
	id := proto.NewULID()
	dir := filepath.Join(stateDir, "jobs", id)
	if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, value any) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("spec.json", proto.NewReceiptSpec(proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/bin/true"}, Limits: proto.DefaultLimits(),
		Outputs:        []proto.OutputSpec{{Path: "result.txt", Collect: proto.OutputCollectAlways, Apply: proto.OutputApplyManual}},
		OutputClientID: testOutputClientID,
	}))
	write("admission.json", proto.Admission{Method: "insecure-test", Time: time.Now()})
	if err := os.Mkdir(filepath.Join(dir, ".outputs-interrupted"), 0o700); err != nil {
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
	if j == nil || j.result == nil || j.result.OutputsOK || j.result.State != proto.StateAmbiguous {
		t.Fatalf("reconciled result = %+v", j)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".outputs-interrupted")); !os.IsNotExist(err) {
		t.Fatalf("interrupted output temp survived restart: %v", err)
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
	outputs, err := outputops.NormalizeSpecs([]proto.OutputSpec{{
		Path: "result.txt", Collect: proto.OutputCollectAlways, Apply: proto.OutputApplyManual,
	}})
	if err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := outputops.Collect(workspace, dir, outputs, true, 1<<20)
	if err != nil || !collected {
		t.Fatalf("Collect() = %+v, %t, %v", bundle, collected, err)
	}
	write := func(name string, value any) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("spec.json", proto.NewReceiptSpec(proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/bin/true"}, Limits: proto.DefaultLimits(), Outputs: outputs,
		OutputClientID: testOutputClientID,
	}))
	write("admission.json", proto.Admission{Method: "insecure-test", Time: time.Now()})
	d, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.mu.Lock()
	j := d.jobs[id]
	d.mu.Unlock()
	if j == nil || j.result == nil || !j.result.OutputsOK || j.result.Outputs == nil ||
		j.result.Outputs.ManifestRoot != bundle.Manifest.RootHash() || j.result.State != proto.StateAmbiguous {
		t.Fatalf("reconciled result = %+v", j)
	}
	req := httptest.NewRequest(http.MethodGet, "/v0/jobs/"+id+"/outputs", nil)
	rec := httptest.NewRecorder()
	d.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("recovered output download = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestValidateSpecRequiresCanonicalBoundedOutputs(t *testing.T) {
	manifest := proto.Manifest{}
	spec := proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/bin/true"}, ManifestRoot: manifest.RootHash(),
		Limits: proto.DefaultLimits(), Outputs: []proto.OutputSpec{{Path: "result.txt"}},
		OutputClientID: testOutputClientID,
	}
	if err := validateSpec(spec, proto.DefaultLimits()); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("noncanonical outputs validation = %v", err)
	}
	normalized, err := outputops.NormalizeSpecs(spec.Outputs)
	if err != nil {
		t.Fatal(err)
	}
	spec.Outputs = normalized
	if err := validateSpec(spec, proto.DefaultLimits()); err != nil {
		t.Fatalf("canonical outputs validation = %v", err)
	}
	spec.Limits.MaxOutputBytes++
	if err := validateSpec(spec, proto.DefaultLimits()); err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("oversized output limit validation = %v", err)
	}
}

func TestOutputByteLimitIsRecorded(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	paths, _, err := snapshot.SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshot.Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	limits := proto.DefaultLimits()
	limits.MaxOutputBytes = 3
	id := proto.NewULID()
	resp := rawSubmitSpec(t, ts.URL, id, root, proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/bin/sh", "-c", "printf large > result.txt"},
		ManifestRoot: manifest.RootHash(), Limits: limits,
		Outputs:        []proto.OutputSpec{{Path: "result.txt", Collect: proto.OutputCollectAlways, Apply: proto.OutputApplyManual}},
		OutputClientID: testOutputClientID,
	}, manifest)
	resp.Body.Close()
	status := waitTerminal(t, ts.URL, id)
	if status.Result == nil || status.Result.OutputsOK || status.Result.LimitExceeded != "output_bytes" {
		t.Fatalf("output limit result = %+v", status.Result)
	}
}

func TestRuntimeLimitAlsoBoundsOutputCollection(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	paths, _, err := snapshot.SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshot.Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	limits := proto.DefaultLimits()
	limits.MaxRuntimeSec = 1
	limits.MaxOutputBytes = 3
	id := proto.NewULID()
	resp := rawSubmitSpec(t, ts.URL, id, root, proto.Spec{
		V: proto.ProtoVersion, Argv: []string{"/bin/sh", "-c", "printf large > result.txt; sleep 30"},
		ManifestRoot: manifest.RootHash(), Limits: limits,
		Outputs:        []proto.OutputSpec{{Path: "result.txt", Collect: proto.OutputCollectAlways, Apply: proto.OutputApplyManual}},
		OutputClientID: testOutputClientID,
	}, manifest)
	resp.Body.Close()
	status := waitTerminal(t, ts.URL, id)
	if status.Result == nil || status.Result.OutputsOK || status.Result.LimitExceeded != "runtime" ||
		!strings.Contains(status.Result.TransactionError, context.DeadlineExceeded.Error()) {
		t.Fatalf("runtime-bounded output collection result = %+v", status.Result)
	}
}

func TestOutputCollectionLimitClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "bytes", err: outputops.ErrLimitExceeded, want: "output_bytes"},
		{name: "deadline", err: fmt.Errorf("packing outputs: %w", context.DeadlineExceeded), want: "runtime"},
		{name: "cancel", err: context.Canceled, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := outputCollectionLimit(test.err); got != test.want {
				t.Fatalf("outputCollectionLimit(%v) = %q, want %q", test.err, got, test.want)
			}
		})
	}
}
