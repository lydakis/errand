package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func TestPersistentWorkspaceRunsKeepFilesAndJobResults(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"value": "initial\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "experiment")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "experiment"); err == nil {
		t.Fatal("creation replaced an existing workspace")
	}
	// Local edits after creation are never implicitly uploaded during reuse.
	if err := os.WriteFile(filepath.Join(root, "value"), []byte("local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(command string) string {
		t.Helper()
		var out, stderr bytes.Buffer
		code := client.Run(client.RunOptions{PeerURL: ts.URL, Root: root, Workspace: "experiment", Argv: []string{"/bin/sh", "-c", command}, Stdout: &out, Stderr: &stderr})
		if code != 0 {
			t.Fatalf("run: %d %s", code, stderr.String())
		}
		return out.String()
	}
	if got := run("cat value; printf 'remote\\n' > value; mkdir ignored; echo reusable > ignored/data; pwd > ignored/cwd"); got != "initial\n" {
		t.Fatalf("first run saw %q", got)
	}
	if got := run("test \"$(cat ignored/cwd)\" = \"$(pwd)\" || exit 9; cat value ignored/data"); got != "remote\nreusable\n" {
		t.Fatalf("second run saw %q", got)
	}
	jobs, err := client.List(ts.URL)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs: %v %v", jobs, err)
	}
	for _, job := range jobs {
		details, err := client.GetJobDetails(ts.URL, job.ID)
		if err != nil || details.Spec.WorkspaceID != ws.ID || details.Result == nil || !details.Result.CleanupOK {
			t.Fatalf("job details: %+v %v", details, err)
		}
		if _, err := os.Stat(filepath.Join(d.jobsDir(), job.ID, "workspace")); !os.IsNotExist(err) {
			t.Fatalf("workspace left in job receipt: %v", err)
		}
	}
	stats, err := client.StorageStats(ts.URL)
	if err != nil || stats.Workspaces == nil || stats.Workspaces.Items != 1 || stats.Workspaces.Bytes == 0 {
		t.Fatalf("workspace storage: %+v %v", stats, err)
	}
	if err := client.RemoveWorkspace(ts.URL, "experiment"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetWorkspace(ts.URL, "experiment"); err == nil {
		t.Fatal("removed workspace still exists")
	}
	// Immutable job results remain fetchable after deleting the live workspace.
	export := filepath.Join(t.TempDir(), "result")
	if _, err := client.FetchChanges(client.ChangeFetchOptions{PeerURL: ts.URL, JobID: jobs[0].ID, OutputDir: export}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(export, "value")); err != nil || string(got) != "remote\n" {
		t.Fatalf("retained result: %q %v", got, err)
	}
}

func TestPersistentWorkspaceExclusiveJobLeaseAndMissingName(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "experiment")
	if err != nil {
		t.Fatal(err)
	}
	id := proto.NewULID()
	spec := proto.Spec{WorkspaceID: ws.ID, Argv: []string{"/bin/sh", "-c", "sleep 30"}, ManifestRoot: ws.Manifest.RootHash(), Selection: ws.Selection, Limits: proto.DefaultLimits()}
	resp := rawSubmitSpec(t, ts.URL, id, root, spec, ws.Manifest)
	if resp.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("submit: %s %s", resp.Status, data)
	}
	resp.Body.Close()
	defer client.Kill(ts.URL, id, true)
	if err := client.RemoveWorkspace(ts.URL, "experiment"); err == nil {
		t.Fatal("removed busy workspace")
	}
	resp = rawSubmitSpec(t, ts.URL, proto.NewULID(), root, spec, ws.Manifest)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second job: %s", resp.Status)
	}
	resp.Body.Close()
	if err := client.Kill(ts.URL, id, true); err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, ts.URL, id)
	if err := client.RemoveWorkspace(ts.URL, "experiment"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := client.Run(client.RunOptions{PeerURL: ts.URL, Root: root, Workspace: "missing", Argv: []string{"true"}, Stdout: &out, Stderr: &out}); code == 0 {
		t.Fatal("missing workspace was implicitly created")
	}
}

func TestPersistentWorkspaceRecoveryReturnsInterruptedJobFiles(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"value": "initial"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "experiment")
	if err != nil {
		t.Fatal(err)
	}
	j := newJob(proto.NewULID(), "")
	j.Dir = filepath.Join(d.jobsDir(), j.ID)
	j.Spec = proto.Spec{WorkspaceID: ws.ID, ManifestRoot: ws.Manifest.RootHash(), Selection: ws.Selection, Argv: []string{"true"}, Limits: proto.DefaultLimits()}
	j.Admission = proto.Admission{Time: time.Now()}
	if err := os.Mkdir(j.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := j.writeJSON("spec.json", proto.NewReceiptSpec(j.Spec)); err != nil {
		t.Fatal(err)
	}
	if err := j.writeJSON("admission.json", j.Admission); err != nil {
		t.Fatal(err)
	}
	if err := d.acquireWorkspace(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(j.workspacePath(), "value"), []byte("unfinished"), 0600); err != nil {
		t.Fatal(err)
	}
	ts.Close()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(d.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	server := httptest.NewServer(restarted.Handler())
	defer server.Close()
	got, err := client.GetWorkspace(server.URL, "experiment")
	if err != nil || got.JobID != "" {
		t.Fatalf("recovered workspace: %+v %v", got, err)
	}
	var output, stderr bytes.Buffer
	if code := client.Run(client.RunOptions{PeerURL: server.URL, Root: root, Workspace: "experiment", Argv: []string{"cat", "value"}, Stdout: &output, Stderr: &stderr}); code != 0 || output.String() != "unfinished" {
		t.Fatalf("recovered contents: %d %q %s", code, output.String(), stderr.String())
	}
}

func TestPersistentWorkspaceSurvivesFailedJobsAndJobGC(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"value": "initial"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "experiment")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := client.Run(client.RunOptions{PeerURL: ts.URL, Root: root, Workspace: ws.Name, Argv: []string{"/bin/sh", "-c", "echo unfinished > value; exit 7"}, Stdout: &out, Stderr: &out})
	if code != 7 {
		t.Fatalf("failed job: %d %s", code, out.String())
	}
	jobs, err := client.List(ts.URL)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs: %v %v", jobs, err)
	}
	// This test deliberately crosses retention's safety window so GC really
	// deletes the receipt, rather than merely verifying a protected-job no-op.
	j := d.jobs[jobs[0].ID]
	j.mu.Lock()
	old := time.Now().Add(-60 * 24 * time.Hour)
	j.result.SettledAt = &old
	j.result.Changes = nil
	if err := replaceJSONDurable(filepath.Join(j.Dir, "result.json"), j.result); err != nil {
		j.mu.Unlock()
		t.Fatal(err)
	}
	j.mu.Unlock()
	keep := 0
	result := postJobGC(t, ts.URL, proto.JobGCRequest{Keep: &keep})
	if result.RemovedJobs != 1 {
		t.Fatalf("GC did not remove receipt: %+v", result)
	}
	if _, err := os.Stat(j.Dir); !os.IsNotExist(err) {
		t.Fatalf("job receipt remains: %v", err)
	}
	out.Reset()
	if code := client.Run(client.RunOptions{PeerURL: ts.URL, Root: root, Workspace: ws.Name, Argv: []string{"cat", "value"}, Stdout: &out, Stderr: io.Discard}); code != 0 || out.String() != "unfinished\n" {
		t.Fatalf("GC damaged persistent workspace: %d %q", code, out.String())
	}
}

func TestPersistentWorkspaceOldReceiptCannotReleaseNewLease(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"value": "keep"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "experiment")
	if err != nil {
		t.Fatal(err)
	}
	makeJob := func() *Job {
		j := newJob(proto.NewULID(), t.TempDir())
		j.Spec = proto.Spec{WorkspaceID: ws.ID, ManifestRoot: ws.Manifest.RootHash(), Selection: ws.Selection}
		j.returnWorkspace = func() error { return d.returnWorkspace(j) }
		return j
	}
	old, current := makeJob(), makeJob()
	if err := d.acquireWorkspace(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	if err := old.cleanupWorkspace(); err != nil {
		t.Fatal(err)
	}
	if err := d.acquireWorkspace(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	// A crash after lease release can leave the earlier receipt unsettled.
	// Recovery must derive its path from the current lease, not its spec.
	recovered := newJob(old.ID, old.Dir)
	recovered.Spec = old.Spec
	recovered.returnWorkspace = func() error { return d.returnWorkspace(recovered) }
	if err := d.restoreWorkspaceLease(recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.workspacePath() == current.workspacePath() {
		t.Fatal("old receipt recovered the newer job's process search path")
	}
	if _, errs := cleanupPersistedRuntime(recovered); len(errs) != 0 {
		t.Fatal(errs)
	}
	// Even an old in-memory Job still holding its former path cannot remove it.
	if err := old.cleanupWorkspace(); err != nil {
		t.Fatal(err)
	}
	got, err := client.GetWorkspace(ts.URL, ws.Name)
	if err != nil || got.JobID != current.ID {
		t.Fatalf("new lease was changed: %+v %v", got, err)
	}
	if data, err := os.ReadFile(filepath.Join(current.workspacePath(), "value")); err != nil || string(data) != "keep" {
		t.Fatalf("new workspace was damaged: %q %v", data, err)
	}
}

func TestPersistentWorkspaceOwnership(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "experiment")
	if err != nil {
		t.Fatal(err)
	}
	owner := Identity{Local: true, LocalUID: 1001}
	stranger := Identity{Local: true, LocalUID: 1002}
	record, err := d.workspaces.read(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Owner = owner.Owner()
	if err := d.workspaces.write(record); err != nil {
		t.Fatal(err)
	}
	d.cfg.InsecureNoAuth = false
	jobID := proto.NewULID()
	d.jobs[jobID] = &Job{ID: jobID, Dir: t.TempDir(), Spec: proto.Spec{WorkspaceID: ws.ID}, Admission: proto.Admission{Method: "local", LocalUID: 1001}, state: proto.StateExited}
	for _, id := range []Identity{stranger, owner} {
		request := httptest.NewRequest("GET", "/v0/workspaces/"+ws.ID, nil)
		request.SetPathValue("id", ws.ID)
		reply := httptest.NewRecorder()
		d.handleWorkspaceGet(reply, request, id)
		want := http.StatusNotFound
		if id.LocalUID == owner.LocalUID {
			want = http.StatusOK
		}
		if reply.Code != want {
			t.Fatalf("ownership get: %d", reply.Code)
		}
		list := httptest.NewRecorder()
		d.handleWorkspaceList(list, httptest.NewRequest("GET", "/v0/workspaces", nil), id)
		var rows []proto.WorkspaceSummary
		if err := json.Unmarshal(list.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		if id.LocalUID == stranger.LocalUID && len(rows) != 0 {
			t.Fatal("listed another owner's workspace")
		}
		storage := httptest.NewRecorder()
		d.handleStorageStats(storage, httptest.NewRequest("GET", "/v0/storage?verbose=1", nil), id)
		var stats proto.StorageStats
		if err := json.Unmarshal(storage.Body.Bytes(), &stats); err != nil || storage.Code != 200 || stats.Details == nil {
			t.Fatalf("storage: %s %v", storage.Body, err)
		}
		jobList := httptest.NewRecorder()
		d.handleList(jobList, httptest.NewRequest("GET", "/v0/jobs?workspace_id="+ws.ID, nil), id)
		var jobs []proto.JobListEntry
		if err := json.Unmarshal(jobList.Body.Bytes(), &jobs); err != nil {
			t.Fatal(err)
		}
		wantCount := 0
		if id.LocalUID == owner.LocalUID {
			wantCount = 1
		}
		if len(stats.Details.Workspaces) != wantCount || len(stats.Details.Jobs) != wantCount || len(jobs) != wantCount {
			t.Fatalf("owner boundary: %+v %+v", stats.Details, jobs)
		}
	}
	request := httptest.NewRequest("DELETE", "/v0/workspaces/"+ws.ID, nil)
	request.SetPathValue("id", ws.ID)
	reply := httptest.NewRecorder()
	d.handleWorkspaceRemove(reply, request, stranger)
	if reply.Code != http.StatusNotFound {
		t.Fatalf("foreign removal: %d %s", reply.Code, reply.Body.String())
	}
	j := newJob(proto.NewULID(), t.TempDir())
	j.Spec = proto.Spec{WorkspaceID: ws.ID, ManifestRoot: ws.Manifest.RootHash()}
	j.Admission = proto.Admission{Method: "local", LocalUID: 1002}
	if err := d.acquireWorkspace(t.Context(), j); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("foreign acquisition: %v", err)
	}
}

func TestPersistentWorkspaceJobFilterPrecedesListCap(t *testing.T) {
	d, ts := testDaemon(t)
	wsID := proto.NewULID()
	jobID := proto.NewULID()
	d.mu.Lock()
	d.jobs[jobID] = &Job{ID: jobID, Spec: proto.Spec{WorkspaceID: wsID}, state: proto.StateExited}
	for range proto.MaxJobListEntries + 1 {
		id := proto.NewULID()
		d.jobs[id] = &Job{ID: id, state: proto.StateRunning}
	}
	d.mu.Unlock()
	jobs, err := client.ListWorkspace(ts.URL, wsID, false)
	if err != nil || len(jobs) != 1 || jobs[0].ID != jobID || jobs[0].WorkspaceID != wsID {
		t.Fatalf("workspace history hidden by newer jobs: %+v %v", jobs, err)
	}
	jobs, err = client.ListWorkspace(ts.URL, wsID, true)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("active filter: %+v %v", jobs, err)
	}
}
