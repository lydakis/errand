package daemon

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func TestConcurrentWorkspaceRecoveryProtectsOnlyUncertainJob(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root, Caches: []proto.CacheBinding{{Name: "compiler", Path: "target"}}}, "experiment")
	if err != nil {
		t.Fatal(err)
	}
	var jobs []*Job
	var done []chan struct{}
	var scopes []scopeRecord
	for range 2 {
		j := newJob(proto.NewULID(), "")
		j.Dir = filepath.Join(d.jobsDir(), j.ID)
		j.Spec = proto.Spec{WorkspaceID: ws.ID, CacheProjectID: ws.CacheProjectID, ManifestRoot: ws.Manifest.RootHash(), Selection: ws.Selection, Argv: []string{"sleep", "60"}, Limits: proto.DefaultLimits()}
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
		if err := d.acquireWorkspace(t.Context(), j); err != nil {
			t.Fatal(err)
		}
		if err := j.prepareNamedCacheTrees(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		scope, err := newProcessScope("")
		if err != nil {
			t.Fatal(err)
		}
		rec := scopeRecord{Token: scope.token, SharedWorkspace: true}
		if err := replaceJSONDurable(filepath.Join(j.Dir, "scope.json"), rec); err != nil {
			t.Fatal(err)
		}
		// Leave a real process for startup recovery, without the old daemon's
		// wait goroutine racing the new daemon's reconciliation.
		cmd := exec.Command("sleep", "60")
		cmd.Dir = filepath.Join(j.workspacePath(), "target")
		cmd.Env = append(os.Environ(), scope.env())
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		rec.Group, err = captureProcessGroup(cmd.Process.Pid)
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal(err)
		}
		if err := replaceJSONDurable(filepath.Join(j.Dir, "scope.json"), rec); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal(err)
		}

		finished := make(chan struct{})
		go func() { _ = cmd.Wait(); close(finished) }()
		t.Cleanup(func() { _ = cmd.Process.Kill(); <-finished })
		jobs, done, scopes = append(jobs, j), append(done, finished), append(scopes, rec)
	}
	if err := os.WriteFile(filepath.Join(jobs[0].Dir, "scope.json"), []byte("{"), 0600); err != nil {
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
	t.Cleanup(func() { _ = restarted.Close() })
	server := httptest.NewServer(restarted.Handler())
	t.Cleanup(server.Close)
	select {
	case <-done[0]:
		t.Fatal("recovery killed sibling whose scope is unknown")
	default:
	}
	select {
	case <-done[1]:
	case <-time.After(3 * time.Second):
		raw, _ := os.ReadFile(filepath.Join(jobs[1].Dir, "events.ndjson"))
		result, _ := os.ReadFile(filepath.Join(jobs[1].Dir, "result.json"))
		t.Fatalf("known job survived recovery: events=%s result=%s", raw, result)
	}
	got, err := client.GetWorkspace(server.URL, ws.Name)
	if err != nil || !slices.Equal(got.JobIDs, []string{jobs[0].ID}) {
		t.Fatalf("protected leases: %+v %v", got, err)
	}
	entries, err := restarted.namedCaches.Inventory(t.Context())
	if err != nil || len(entries) != 1 || !slices.Equal(entries[0].Holders, []string{jobs[0].ID}) {
		t.Fatalf("protected cache released: %+v %v", entries, err)
	}
	if err := client.RemoveWorkspace(server.URL, ws.Name); err == nil {
		t.Fatal("removed workspace with protected survivor")
	}
	// Repair the damaged scope and retry recovery. The final member releases
	// the shared cache only after its own process is gone.
	if err := replaceJSONDurable(filepath.Join(jobs[0].Dir, "scope.json"), scopes[0]); err != nil {
		t.Fatal(err)
	}
	server.Close()
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	final, err := New(d.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	select {
	case <-done[0]:
	case <-time.After(3 * time.Second):
		t.Fatal("repaired job survived recovery")
	}
	row, err := final.workspaces.read(ws.ID)
	if err != nil || len(row.JobIDs) != 0 {
		t.Fatalf("final leases: %+v %v", row, err)
	}
	entries, err = final.namedCaches.Inventory(t.Context())
	if err != nil || len(entries) != 1 || entries[0].Protected() {
		t.Fatalf("final cache still leased: %+v %v", entries, err)
	}
}
