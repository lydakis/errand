package daemon

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func TestPersistentCacheReplacement(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	_, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	if _, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root, Caches: []proto.CacheBinding{{Name: "compiler", Path: "target"}}}, "experiment"); err != nil {
		t.Fatal(err)
	}
	for _, script := range []string{"rm -rf target && mkdir target && echo preserved > target/output", "test -L target && test ! -e target/output && test \"$(cat .errand-cache-recovery-*/compiler/output)\" = preserved"} {
		var out bytes.Buffer
		if code := client.Run(client.RunOptions{PeerURL: ts.URL, Root: root, Workspace: "experiment", Argv: []string{"/bin/sh", "-c", script}, Stdout: &out, Stderr: &out}); code != 0 {
			t.Fatalf("run %q failed: %d %s", script, code, &out)
		}
	}
}

func TestTerminalPreScopeLeaseRecovery(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, nil)
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
	// Model the durable receipt left by abortAdmission when releasing a
	// never-started job's lease failed. There is deliberately no scope.json.
	now := time.Now()
	if err := j.writeJSON("result.json", &proto.Result{State: proto.StateAmbiguous, StartError: "job was rejected before execution", SettledAt: &now, CleanupOK: false, ChangesOK: true, LogsComplete: true, TransactionError: "cleaning rejected admission: transient write failure"}); err != nil {
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
	if err != nil {
		t.Fatal(err)
	}
	if got.JobID != "" {
		t.Fatalf("restart left never-started job lease busy: %s", got.JobID)
	}
}

func TestCorruptWorkspaceDoesNotBreakEphemeralCleanup(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "experiment")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.workspaces.dir, ws.ID, "workspace.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := client.Run(client.RunOptions{PeerURL: ts.URL, Root: root, Argv: []string{"true"}, Stdout: &out, Stderr: &out}); code != 0 {
		t.Fatalf("unrelated ephemeral run failed: %d %s", code, &out)
	}
	ts.Close()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(d.cfg)
	if err != nil {
		t.Fatalf("corrupt workspace blocked startup: %v", err)
	}
	defer restarted.Close()
	server := httptest.NewServer(restarted.Handler())
	defer server.Close()
	out.Reset()
	if code := client.Run(client.RunOptions{PeerURL: server.URL, Root: root, Argv: []string{"true"}, Stdout: &out, Stderr: &out}); code != 0 {
		t.Fatalf("run after restart: %d %s", code, &out)
	}
}
