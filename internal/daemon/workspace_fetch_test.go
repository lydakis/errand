package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func TestWorkspaceFetchCancelledJobPreservesFilesAndCheckpoint(t *testing.T) {
	d, ts := concurrencyDaemon(t, 1, 2)
	root := workspaceWith(t, map[string]string{"value": "initial\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "cancelled-fetch")
	if err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(d.workspaces.dir, ws.ID, "data", "value")
	writeRemote := func(body string) {
		t.Helper()
		if err := os.WriteFile(remote, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(detach bool) string {
		t.Helper()
		var out bytes.Buffer
		if code := client.Run(client.RunOptions{PeerURL: ts.URL, Root: root, Workspace: ws.Name,
			Argv: []string{"true"}, Detach: detach, Stdout: &out, Stderr: &out}); code != 0 {
			t.Fatalf("run: %d %s", code, &out)
		}
		jobs, err := client.ListWorkspace(ts.URL, ws.ID, false)
		if err != nil || len(jobs) == 0 {
			t.Fatalf("jobs: %v %v", jobs, err)
		}
		return jobs[0].ID
	}
	fetch := func(id string) error {
		t.Helper()
		_, err := client.FetchChanges(client.ChangeFetchOptions{PeerURL: ts.URL, JobID: id, Apply: true, CallerDir: root})
		return err
	}
	assertLocal := func(want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(root, "value"))
		if err != nil || string(got) != want {
			t.Fatalf("local value: %q, want %q: %v", got, want, err)
		}
	}

	writeRemote("accepted remote edit\n")
	if err := fetch(run(false)); err != nil {
		t.Fatal(err)
	}
	assertLocal("accepted remote edit\n")

	blocker := proto.NewULID()
	resp := rawSubmit(t, ts.URL, blocker, root, []string{"/bin/sleep", "30"})
	resp.Body.Close()
	t.Cleanup(func() {
		forceKill(t, ts.URL, blocker)
		waitTerminal(t, ts.URL, blocker)
	})
	queued := run(true)
	waitState(t, ts.URL, queued, proto.StateQueued)
	forceKill(t, ts.URL, queued)
	status := waitTerminal(t, ts.URL, queued)
	if status.Result.Started || !status.Result.ChangesOK || status.Result.Changes != nil {
		t.Fatalf("cancelled job: %+v", status.Result)
	}
	if err := fetch(queued); err == nil || !strings.Contains(err.Error(), "did not run") {
		t.Errorf("fetch of cancelled job: %v", err)
	}
	assertLocal("accepted remote edit\n")

	forceKill(t, ts.URL, blocker)
	waitTerminal(t, ts.URL, blocker)
	// A genuinely observed empty result must still undo the accepted edit.
	// This also detects a checkpoint advanced by the cancelled job's fetch.
	writeRemote("initial\n")
	if err := fetch(run(false)); err != nil {
		t.Fatal(err)
	}
	assertLocal("initial\n")
}
