package daemon

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
)

func TestWorkspaceGateWaitHonorsCancellation(t *testing.T) {
	s := &workspaceStore{}
	unlock := s.lockWorkspace("workspace")
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if release, err := s.lockWorkspaceContext(ctx, "workspace"); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("gate ignored cancellation: %v", err)
	}
	s.gateMu.Lock()
	defer s.gateMu.Unlock()
	if s.gates["workspace"].users != 1 {
		t.Fatal("canceled waiter retained a gate reference")
	}
}

func TestTransferGCCancellationStopsBeforeNextWorkspace(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"value": "initial\n"})
	first, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "a-first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.PushChanges(client.PushOptions{PeerURL: ts.URL, Root: root, Workspace: first.Name}); err != nil {
		t.Fatal(err)
	}
	second, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "b-second")
	if err != nil {
		t.Fatal(err)
	}
	unlock := d.workspaces.lockWorkspace(second.ID)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", "/v0/gc/changes", strings.NewReader(`{"older_than_seconds":1}`)).WithContext(ctx)
	done := make(chan struct{})
	go func() { defer close(done); d.handleTransferGC(httptest.NewRecorder(), req, Identity{}) }()
	select {
	case <-done:
		unlock()
	case <-time.After(500 * time.Millisecond):
		unlock()
		<-done
		t.Fatal("canceled GC proceeded to wait on a later workspace gate")
	}
}
