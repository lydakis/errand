package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func TestLegacyWorkspaceRecoveryKeepsExclusiveProcessScope(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "experiment")
	if err != nil {
		t.Fatal(err)
	}
	j := newJob(proto.NewULID(), "")
	j.Dir = filepath.Join(d.jobsDir(), j.ID)
	j.Spec = proto.Spec{WorkspaceID: ws.ID, ManifestRoot: ws.Manifest.RootHash(), Selection: ws.Selection, Argv: []string{"sleep", "60"}, Limits: proto.DefaultLimits()}
	if err := os.Mkdir(j.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := j.writeJSON("spec.json", proto.NewReceiptSpec(j.Spec)); err != nil {
		t.Fatal(err)
	}
	row, err := d.workspaces.read(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The previous format had one job_id, no member array, and no group ID.
	row.JobID = j.ID
	if err := d.workspaces.write(row); err != nil {
		t.Fatal(err)
	}
	contender := newJob(proto.NewULID(), t.TempDir())
	contender.Spec = j.Spec
	if err := d.acquireWorkspace(t.Context(), contender); !errors.Is(err, errWorkspaceBusy) {
		t.Fatalf("joined an unrecovered exclusive workspace: %v", err)
	}
	data := filepath.Join(d.workspaces.dir, ws.ID, "data")
	scope, err := newProcessScope(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.writeJSON("scope.json", scopeRecord{Token: scope.token}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	cmd.Dir = data
	cmd.Env = []string{} // Legacy recovery must still use exclusive cwd evidence.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	ts.Close()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(d.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("legacy process survived recovery")
	}
	row, err = restarted.workspaces.read(ws.ID)
	if err != nil || row.LegacyExclusive || len(row.JobIDs) != 0 {
		t.Fatalf("legacy lease not cleared: %+v %v", row, err)
	}
	if err := restarted.acquireWorkspace(t.Context(), contender); err != nil {
		t.Fatalf("recovered workspace cannot be reused: %v", err)
	}
	if err := restarted.returnWorkspace(contender); err != nil {
		t.Fatal(err)
	}
}
