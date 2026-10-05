package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/daemon"
	"github.com/lydakis/errand/internal/proto"
)

// recreateFixture starts a runner, configures it as peer "test" with profile
// "dev", and creates workspace api from a checkout with artifact dist. A job
// then leaves a runner-only ignored file and an artifact in the tree.
func recreateFixture(t *testing.T) (url, state, root string, ws proto.Workspace, jobID string) {
	t.Helper()
	state = t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	server := httptest.NewServer(d.Handler())
	t.Cleanup(server.Close)
	writeClientConfig(t, fmt.Sprintf("[peers.test]\nurl = %q\n[profiles.dev.run]\npeer = 'test'\nworkspace = 'api'\n", server.URL))
	root = t.TempDir()
	t.Chdir(root)
	for name, body := range map[string]string{".errandignore": "build/\n", "value": "initial\n"} {
		if err := os.WriteFile(name, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out, stderr bytes.Buffer
	if code := cmdWorkspacesTo([]string{"create", "--on", "test", "--artifact", "dist", "api"}, &out, &stderr); code != 0 {
		t.Fatalf("create: %d %s", code, &stderr)
	}
	if ws, err = client.GetWorkspace(server.URL, "api"); err != nil {
		t.Fatal(err)
	}
	job := "mkdir -p build dist && echo built > dist/out && echo cached > build/cache && echo job > value"
	if code := client.Run(client.RunOptions{PeerURL: server.URL, Root: root, Workspace: "api", Argv: []string{"sh", "-c", job}, Stdout: &out, Stderr: &stderr}); code != 0 {
		t.Fatalf("job: %d %s", code, &stderr)
	}
	return server.URL, state, root, ws, onlyJob(t, server.URL, ws.ID)
}

func onlyJob(t *testing.T, url, workspaceID string) string {
	t.Helper()
	jobs, err := client.ListWorkspace(url, workspaceID, false)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs: %+v %v", jobs, err)
	}
	return jobs[0].ID
}

// recordEarlierTransferState rewrites the workspace's relationship as an
// earlier errand recorded it: the creation manifest embedded in origin.json,
// no initial_root and no initial.json.
func recordEarlierTransferState(t *testing.T, state, workspaceID string) string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(state, "errand", "workspace-transfers", "*-"+workspaceID))
	if err != nil || len(dirs) != 1 {
		t.Fatalf("transfer state: %v %v", dirs, err)
	}
	dir := dirs[0]
	var origin map[string]json.RawMessage
	raw, err := os.ReadFile(filepath.Join(dir, "origin.json"))
	if err == nil {
		err = json.Unmarshal(raw, &origin)
	}
	if err == nil {
		origin["initial"], err = os.ReadFile(filepath.Join(dir, "initial.json"))
	}
	if err != nil {
		t.Fatal(err)
	}
	delete(origin, "initial_root")
	if raw, err = json.Marshal(origin); err == nil {
		err = os.WriteFile(filepath.Join(dir, "origin.json"), raw, 0600)
	}
	if err == nil {
		err = os.Remove(filepath.Join(dir, "initial.json"))
	}
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func runInWorkspace(t *testing.T, url, root string, argv ...string) (int, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := client.Run(client.RunOptions{PeerURL: url, Root: root, Workspace: "api", Argv: argv, Stdout: &out, Stderr: &stderr})
	return code, out.String()
}

func TestRecreateEarlierWorkspaceShowsLossesBeforeRemoving(t *testing.T) {
	url, state, root, ws, jobID := recreateFixture(t)
	dir := recordEarlierTransferState(t, state, ws.ID)
	var out, stderr bytes.Buffer

	// Every command that meets the earlier state names the preview, as typed.
	for _, c := range []struct {
		args []string
		run  func([]string, *bytes.Buffer, *bytes.Buffer) int
		want string
	}{
		{[]string{"--on", "test", "--workspace", "api"}, push, "errand workspaces recreate --on test api"},
		{[]string{"--on", "test", "--workspace", "api", "--watch"}, push, "errand workspaces recreate --on test api"},
		{[]string{"--profile", "dev"}, push, "errand workspaces recreate --on test api"},
		{[]string{"--url", url, "--workspace", "api"}, push, "errand workspaces recreate --url '" + url + "' api"},
		{[]string{"--apply", "test/" + jobID}, fetch, "errand workspaces recreate --on test " + ws.ID},
		{[]string{"changes", "--older-than", "1d"}, gc, "errand workspaces recreate --url '" + url + "' " + ws.ID},
	} {
		stderr.Reset()
		if code := c.run(c.args, &out, &stderr); code == 0 || !strings.Contains(stderr.String(), "created by an earlier errand") ||
			!strings.Contains(stderr.String(), "Recreate it from "+root+". This shows what recreating deletes on the runner and how to keep it, and removes nothing:") || !strings.Contains(stderr.String(), c.want) {
			t.Fatalf("%v: %d %s", c.args, code, &stderr)
		}
	}

	// The preview removes nothing and says what would be lost and how to keep it.
	stderr.Reset()
	if code := cmdWorkspacesTo([]string{"recreate", "--on", "test", "api"}, &out, &stderr); code != 0 {
		t.Fatalf("preview: %d %s", code, &stderr)
	}
	for _, want := range []string{
		"Recreating workspace api on test removes it and creates it again from " + root + ".",
		"Deleted on the runner: its working tree (",
		"The new workspace keeps the same artifacts dist.",
		"Its earlier job keeps its retained result until it expires, but this version cannot apply it with fetch --apply. Export with:\n  errand fetch --output DIR test/" + jobID,
		"  errand --on test --workspace api --no-apply -- true\n",
		"Nothing was removed. To recreate it, run:\n  errand workspaces recreate --on test --yes api\n",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("preview lacks %q:\n%s", want, &stderr)
		}
	}
	if now, err := client.GetWorkspace(url, "api"); err != nil || now.ID != ws.ID {
		t.Fatalf("preview changed the workspace: %+v %v", now, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "origin.json")); err != nil {
		t.Fatalf("preview changed local state: %v", err)
	}
	out.Reset()
	if code := cmdWorkspacesTo([]string{"recreate", "--on", "test", "--json", "api"}, &out, &stderr); code != 0 {
		t.Fatalf("json preview: %d %s", code, &stderr)
	}
	var preview recreationPreview
	if err := json.Unmarshal(out.Bytes(), &preview); err != nil || preview.ID != ws.ID || preview.Root != root || !preview.EarlierState ||
		preview.Removed || !slices.Equal(preview.Jobs, []string{jobID}) || preview.WorkingBytes <= 0 {
		t.Fatalf("json preview: %+v %v", preview, err)
	}

	// The capture the preview suggests works with the earlier state in place.
	if code, _ := runInWorkspace(t, url, root, "true"); code != 0 {
		t.Fatalf("capture job: %d", code)
	}
	jobs, err := client.ListWorkspace(url, ws.ID, false)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs: %+v %v", jobs, err)
	}
	capture := jobs[0].ID
	if capture == jobID {
		capture = jobs[1].ID
	}
	exported := filepath.Join(t.TempDir(), "capture")
	if _, err := client.FetchChanges(client.ChangeFetchOptions{PeerURL: url, JobID: capture, OutputDir: exported}); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(exported, "dist", "out")); err != nil || string(body) != "built\n" {
		t.Fatalf("captured artifact: %q %v", body, err)
	}

	out.Reset()
	stderr.Reset()
	if code := cmdWorkspacesTo([]string{"recreate", "--on", "test", "--yes", "api"}, &out, &stderr); code != 0 || out.String() != "api\n" ||
		!strings.Contains(stderr.String(), "recreated workspace api on test") {
		t.Fatalf("recreate: %d %q %s", code, &out, &stderr)
	}
	now, err := client.GetWorkspace(url, "api")
	if err != nil || now.ID == ws.ID || !slices.Equal(now.Selection.Artifacts, []string{"dist"}) {
		t.Fatalf("recreated: %+v %v", now, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("earlier transfer state kept: %v", err)
	}
	if code, value := runInWorkspace(t, url, root, "sh", "-c", "cat value; test ! -e build/cache"); code != 0 || value != "initial\n" {
		t.Fatalf("recreated tree: %d %q", code, value)
	}
	// Retained results outlive the workspace.
	if _, err := client.FetchChanges(client.ChangeFetchOptions{PeerURL: url, JobID: jobID, OutputDir: filepath.Join(t.TempDir(), "old")}); err != nil {
		t.Fatalf("export after recreation: %v", err)
	}
	if err := os.WriteFile("value", []byte("local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := cmdPushTo([]string{"--profile", "dev", "--apply"}, &out, &stderr); code != 0 {
		t.Fatalf("push after recreation: %d %s", code, &stderr)
	}
	if code, value := runInWorkspace(t, url, root, "cat", "value"); code != 0 || value != "local\n" {
		t.Fatalf("pushed: %d %q", code, value)
	}
	out.Reset()
	stderr.Reset()
	if code := cmdGCTo([]string{"changes", "--older-than", "1d"}, &out, &stderr); code != 0 || !strings.HasSuffix(out.String(), "0 failed)\n") {
		t.Fatalf("gc after recreation: %d %q %q", code, &out, &stderr)
	}
}

func TestRecreateKeepsCurrentJobsApplicable(t *testing.T) {
	url, _, root, ws, jobID := recreateFixture(t)
	var out, stderr bytes.Buffer
	if code := cmdWorkspacesTo([]string{"recreate", "--on", "test", "api"}, &out, &stderr); code != 0 ||
		!strings.Contains(stderr.String(), "Its earlier job keeps its retained result until it expires; fetch --apply and fetch --output still work.") {
		t.Fatalf("preview: %d %s", code, &stderr)
	}
	if code := cmdWorkspacesTo([]string{"recreate", "--on", "test", "--yes", "api"}, &out, &stderr); code != 0 {
		t.Fatalf("recreate: %d %s", code, &stderr)
	}
	if now, err := client.GetWorkspace(url, "api"); err != nil || now.ID == ws.ID {
		t.Fatalf("recreated: %+v %v", now, err)
	}
	if _, err := client.FetchChanges(client.ChangeFetchOptions{PeerURL: url, JobID: jobID, Apply: true, CallerDir: root}); err != nil {
		t.Fatalf("apply after recreation: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(root, "value")); err != nil || string(body) != "job\n" {
		t.Fatalf("applied: %q %v", body, err)
	}
}

func TestRecreateRefusesBeforeRemoving(t *testing.T) {
	url, _, root, ws, _ := recreateFixture(t)
	var out, stderr bytes.Buffer
	if code := client.Run(client.RunOptions{PeerURL: url, Root: root, Workspace: "api", Argv: []string{"sleep", "30"}, Detach: true, Stdout: &out, Stderr: &stderr}); code != 0 {
		t.Fatalf("detach: %d %s", code, &stderr)
	}
	busy := strings.TrimSpace(out.String())
	busy = busy[strings.LastIndexByte(busy, '/')+1:]
	stderr.Reset()
	if code := cmdWorkspacesTo([]string{"recreate", "--on", "test", "--yes", "api"}, &out, &stderr); code == 0 ||
		!strings.Contains(stderr.String(), "workspace api is in use by "+busy+"; recreate it after they finish") {
		t.Fatalf("busy: %d %s", code, &stderr)
	}
	if err := client.Kill(url, busy, true); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if now, err := client.GetWorkspace(url, "api"); err != nil || len(now.JobIDs) == 0 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("killed job still holds the workspace: %v", now.JobIDs)
		}
	}
	// A snapshot the checkout refuses stops recreation before removal.
	requireFIFOs(t)
	if err := mkfifo(filepath.Join(root, "unsupported")); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := cmdWorkspacesTo([]string{"recreate", "--on", "test", "--yes", "api"}, &out, &stderr); code == 0 || !strings.Contains(stderr.String(), "unsupported") {
		t.Fatalf("recreated from a checkout the snapshot refuses: %d %s", code, &stderr)
	}
	if now, err := client.GetWorkspace(url, "api"); err != nil || now.ID != ws.ID {
		t.Fatalf("refused recreation removed the workspace: %+v %v %s", now, err, &stderr)
	}
}

func push(args []string, out, stderr *bytes.Buffer) int  { return cmdPushTo(args, out, stderr) }
func fetch(args []string, out, stderr *bytes.Buffer) int { return cmdFetchTo(args, out, stderr) }
func gc(args []string, out, stderr *bytes.Buffer) int    { return cmdGCTo(args, out, stderr) }
