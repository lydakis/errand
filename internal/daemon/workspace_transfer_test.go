package daemon

import (
	"bytes"
	"context"
	"errors"
	changeops "github.com/lydakis/errand/internal/changes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func TestPushRecoversCollectedStageFromFrozenSource(t *testing.T) {
	d, _ := testDaemon(t)
	var uploads atomic.Int32
	var interrupted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/push") {
			uploads.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/apply") && !interrupted.Swap(true) {
			d.workspaces.mu.Lock()
			rows, err := d.workspaces.records()
			d.workspaces.mu.Unlock()
			if err != nil {
				t.Error(err)
			}
			for _, row := range rows {
				entries, err := os.ReadDir(filepath.Join(d.workspaces.dir, row.ID, "push"))
				if err != nil {
					t.Error(err)
				}
				for _, entry := range entries {
					gc, err := d.pushSession(row, entry.Name()).GC(context.Background(), time.Now().Add(time.Hour), false, []proto.Manifest{row.Manifest})
					if err != nil || gc.Removed != 1 {
						t.Errorf("GC: %+v %v", gc, err)
					}
				}
			}
			http.Error(w, "connection lost before apply", http.StatusBadGateway)
			return
		}
		d.Handler().ServeHTTP(w, r)
	}))
	defer server.Close()
	root := workspaceWith(t, map[string]string{"value": "initial\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: server.URL, Root: root}, "collected")
	if err != nil {
		t.Fatal(err)
	}
	write := func(value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "value"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("frozen\n")
	var stats client.TransferStats
	opts := client.PushOptions{PeerURL: server.URL, Workspace: ws.Name, Root: root, Stats: &stats}
	if _, err := client.PushChanges(opts); err != nil {
		t.Fatal(err)
	}
	opts.Apply = true
	if _, err := client.PushChanges(opts); err == nil || !strings.Contains(err.Error(), "repeat push to recover") {
		t.Fatalf("lost reply: %v", err)
	}
	if uploads.Load() != 1 {
		t.Fatalf("applying acknowledged stage re-uploaded: %d", uploads.Load())
	}
	if stats.TransferredBytes != 0 {
		t.Fatalf("acknowledged stage counted another upload: %+v", stats)
	}
	write("new local edits\n")
	opts.Apply = false
	result, err := client.PushChanges(opts)
	if err != nil || !result.Recovered {
		t.Fatalf("recovery: %+v %v", result, err)
	}
	if stats.TransferredBytes <= 0 || stats.ChangedPaths != 1 {
		t.Fatalf("recovery omitted frozen-source reupload: %+v", stats)
	}
	remote := filepath.Join(d.workspaces.dir, ws.ID, "data", "value")
	if body, err := os.ReadFile(remote); err != nil || string(body) != "frozen\n" {
		t.Fatalf("wrong source applied: %q %v", body, err)
	}
	if uploads.Load() != 2 {
		t.Fatalf("recovery uploads=%d", uploads.Load())
	}
	opts.Apply = true
	if _, err := client.PushChanges(opts); err != nil {
		t.Fatalf("next push remains blocked: %v", err)
	}
	if body, err := os.ReadFile(remote); err != nil || string(body) != "new local edits\n" {
		t.Fatalf("next push: %q %v", body, err)
	}
}

func TestWorkspaceTransferRoundTrip(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"value": "one\ntwo\nthree\n", "other": "initial\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "iteration")
	if err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(d.workspaces.dir, ws.ID, "data")
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(dir, name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	run := func() string {
		t.Helper()
		var out, stderr bytes.Buffer
		if code := client.Run(client.RunOptions{PeerURL: ts.URL, Root: root, Workspace: ws.Name, Argv: []string{"true"}, Stdout: &out, Stderr: &stderr}); code != 0 {
			t.Fatalf("run: %d %s", code, stderr.String())
		}
		jobs, err := client.ListWorkspace(ts.URL, ws.ID, false)
		if err != nil || len(jobs) == 0 {
			t.Fatalf("jobs: %v %v", jobs, err)
		}
		return jobs[0].ID
	}
	fetch := func(job string) {
		t.Helper()
		if _, err := client.FetchChanges(client.ChangeFetchOptions{PeerURL: ts.URL, JobID: job, Apply: true, CallerDir: root}); err != nil {
			t.Fatal(err)
		}
	}
	write(remote, "value", "remote\ntwo\nthree\n")
	fetch(run())
	write(root, "value", "remote\ntwo\nlocal\n")
	push := client.PushOptions{PeerURL: ts.URL, Workspace: ws.Name, Root: root}
	if _, err := client.PushChanges(push); err != nil {
		t.Fatal(err)
	}
	if got := read(remote, "value"); got != "remote\ntwo\nthree\n" {
		t.Fatalf("staging changed destination: %q", got)
	}
	push.Apply = true
	if _, err := client.PushChanges(push); err != nil {
		t.Fatal(err)
	}
	if got := read(remote, "value"); got != "remote\ntwo\nlocal\n" {
		t.Fatalf("push result: %q", got)
	}
	write(remote, "value", "again\ntwo\nlocal\n")
	fetch(run())
	if got := read(root, "value"); got != "again\ntwo\nlocal\n" {
		t.Fatalf("second fetch: %q", got)
	}
	write(root, "value", "again\ntwo\nnew local\n")
	if _, err := client.PushChanges(push); err != nil {
		t.Fatal(err)
	}
	if got := read(remote, "value"); got != "again\ntwo\nnew local\n" {
		t.Fatalf("second push: %q", got)
	}
}

func TestWorkspacePushConflictAndLostResponse(t *testing.T) {
	d, _ := testDaemon(t)
	var dropped atomic.Bool
	var inject atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if inject.Load() && strings.HasSuffix(r.URL.Path, "/apply") && !dropped.Swap(true) {
			recorder := httptest.NewRecorder()
			d.Handler().ServeHTTP(recorder, r)
			if recorder.Code != 200 {
				t.Errorf("first apply: %d %s", recorder.Code, recorder.Body.String())
			}
			http.Error(w, "lost response", 502)
			return
		}
		d.Handler().ServeHTTP(w, r)
	}))
	defer server.Close()
	root := workspaceWith(t, map[string]string{"value": "initial\n", "clean": "initial\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: server.URL, Root: root}, "retry")
	if err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(d.workspaces.dir, ws.ID, "data")
	write := func(dir, name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(root, "value", "local\n")
	write(remote, "value", "remote\n")
	write(root, "clean", "clean change\n")
	var stats client.TransferStats
	opts := client.PushOptions{PeerURL: server.URL, Root: root, Workspace: ws.Name, Apply: true, Stats: &stats}
	_, err = client.PushChanges(opts)
	var conflict *changeops.MergeConflictError
	if !errors.As(err, &conflict) || conflict.Materialized {
		t.Fatalf("refusal: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(remote, "clean")); string(got) != "initial\n" {
		t.Fatalf("refusal wrote clean sibling: %q", got)
	}
	opts.MaterializeConflicts = true
	if _, err := client.PushChanges(opts); !errors.As(err, &conflict) || !conflict.Materialized {
		t.Fatalf("materialization: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(remote, "clean")); string(got) != "clean change\n" {
		t.Fatalf("clean sibling missing: %q", got)
	}
	// Resolve locally and remotely, then exercise a response lost AFTER application.
	write(root, "value", "resolved\n")
	write(remote, "value", "resolved\n")
	opts.MaterializeConflicts = false
	inject.Store(true)
	if _, err := client.PushChanges(opts); err == nil {
		t.Fatal("expected lost response")
	}
	write(remote, "value", "job edited after apply\n")
	if _, err := client.PushChanges(opts); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if stats.TransferredBytes != 0 {
		t.Fatalf("receipt-only recovery counted an upload: %+v", stats)
	}
	if got, _ := os.ReadFile(filepath.Join(remote, "value")); string(got) != "job edited after apply\n" {
		t.Fatalf("retry overwrote later edit: %q", got)
	}
}

func TestWorkspaceFetchReturnToCreationAndGC(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"value": "initial\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "reset")
	if err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(d.workspaces.dir, ws.ID, "data")
	runFetch := func() {
		t.Helper()
		var out bytes.Buffer
		if code := client.Run(client.RunOptions{PeerURL: ts.URL, Root: root, Workspace: ws.Name, Argv: []string{"true"}, Stdout: &out, Stderr: &out}); code != 0 {
			t.Fatal(out.String())
		}
		jobs, err := client.ListWorkspace(ts.URL, ws.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.FetchChanges(client.ChangeFetchOptions{PeerURL: ts.URL, JobID: jobs[0].ID, Apply: true, CallerDir: root}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(remote, "value"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runFetch()
	before, err := client.ChangeStats()
	if err != nil {
		t.Fatal(err)
	}
	// A later job with no creation-relative changes still has a source version.
	if err := os.WriteFile(filepath.Join(remote, "value"), []byte("initial\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runFetch()
	if got, _ := os.ReadFile(filepath.Join(root, "value")); string(got) != "initial\n" {
		t.Fatalf("zero-result fetch: %q", got)
	}
	if before.Bytes == 0 {
		t.Fatal("transfer state absent from df")
	}
	if _, err := client.RemoteTransferGC(ts.URL, time.Second, true); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceFetchDoesNotDeleteUnobservedArtifacts(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{".errandignore": "out/\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root, Artifacts: []string{"out"}}, "artifacts")
	if err != nil {
		t.Fatal(err)
	}
	run := func(command string, artifacts []string) {
		t.Helper()
		var out bytes.Buffer
		if code := client.Run(client.RunOptions{PeerURL: ts.URL, Root: root, Workspace: ws.Name, ApplyOnSuccess: true, Artifacts: artifacts, Argv: []string{"sh", "-c", command}, Stdout: &out, Stderr: &out}); code != 0 {
			t.Fatalf("run: %d %s", code, &out)
		}
	}
	run("mkdir out; echo report > out/report", nil)
	if got, err := os.ReadFile(filepath.Join(root, "out", "report")); err != nil || string(got) != "report\n" {
		t.Fatalf("first artifact: %q %v", got, err)
	}
	run("true", []string{})
	if got, err := os.ReadFile(filepath.Join(root, "out", "report")); err != nil || string(got) != "report\n" {
		t.Fatalf("omitted declaration deleted artifact: %q %v", got, err)
	}
}

func TestWorkspacePushOwnershipAndImmutableTarget(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "owned")
	if err != nil {
		t.Fatal(err)
	}
	row, err := d.workspaces.read(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner := Identity{Local: true, LocalUID: 1001}
	stranger := Identity{Local: true, LocalUID: 1002}
	row.Owner = owner.Owner()
	if err := d.workspaces.write(row); err != nil {
		t.Fatal(err)
	}
	d.cfg.InsecureNoAuth = false
	for _, handler := range []func(http.ResponseWriter, *http.Request, Identity){d.handleWorkspacePush, d.handleWorkspacePushApply} {
		r := httptest.NewRequest("POST", "/", nil)
		r.SetPathValue("id", ws.ID)
		w := httptest.NewRecorder()
		handler(w, r, stranger)
		if w.Code != 404 {
			t.Fatalf("other owner reached push: %d %s", w.Code, w.Body.String())
		}
		r = httptest.NewRequest("POST", "/", nil)
		r.SetPathValue("id", ws.Name)
		w = httptest.NewRecorder()
		handler(w, r, owner)
		if w.Code != 404 {
			t.Fatalf("mutation accepted mutable name: %d", w.Code)
		}
	}
}
