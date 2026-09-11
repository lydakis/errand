package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWorkspaceTransferDoesNotReenterDownloadLock(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	prep := prepareSnapshot(root, true, false)
	if prep.err != nil {
		t.Fatal(prep.err)
	}
	const peer, id = "http://runner", "01M2280R0T4152A3BSV4C2976R"
	if err := recordWorkspaceOrigin(RunOptions{PeerURL: peer, Root: root}, id, prep.manifest); err != nil {
		t.Fatal(err)
	}
	dir, err := workspaceTransferDir(peer, id)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce the old shared-namespace collision with a real job key,
	// without relying on randomly generated IDs or changing the hash function.
	want := localChangeTransferLockName("workspace-" + filepath.Base(dir))
	var name string
	for i := 0; i < 65536; i++ {
		candidate := localChangeTransferLockName(localChangeKey(peer, fmt.Sprintf("%026d", i)))
		if candidate == want {
			name = candidate
			break
		}
	}
	if name == "" {
		t.Fatal("could not construct download/workspace lock collision")
	}
	unlock, err := acquireLocalChangeLock(name)
	if err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(unlock)
	defer release()
	done := make(chan error, 1)
	go func() {
		// Fetch/apply holds the job download lock through checkout recovery
		// and subsequent workspace transfer staging/application.
		done <- withWorkspaceChangeLock(root, func() error {
			if err := recoverWorkspaceApplications(root); err != nil {
				return err
			}
			unlock, err := lockWorkspaceTransfer(dir)
			if err != nil {
				return err
			}
			unlock()
			return nil
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		release()
		<-done // Join the waiter before removing its temporary state.
		t.Fatal("workspace transfer waited on the already-held job download lock")
	}
}

func TestRejectedWorkspaceCreationReclaimsOrigin(t *testing.T) {
	for _, status := range []int{409, 502} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "data"), []byte(strings.Repeat("x", 32768)), 0600); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "creation failed", status) }))
			defer server.Close()
			if _, err := CreateWorkspace(RunOptions{PeerURL: server.URL, Root: root, IncludeAll: true}, "duplicate"); err == nil {
				t.Fatal("expected failure")
			}
			stats, err := workspaceTransferStats(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if status == 409 && stats.Bytes != 0 {
				t.Fatalf("rejected creation retains %d bytes", stats.Bytes)
			}
			if status == 502 && stats.Bytes == 0 {
				t.Fatal("uncertain creation lost its origin")
			}
		})
	}
}

func TestTransferGCReportsCorruptCheckpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "data"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	prep := prepareSnapshot(root, true, false)
	if prep.err != nil {
		t.Fatal(prep.err)
	}
	const id = "01M2280R0T4152A3BSV4C2976R"
	opts := RunOptions{PeerURL: "http://runner", Root: root}
	if err := recordWorkspaceOrigin(opts, id, prep.manifest); err != nil {
		t.Fatal(err)
	}
	dir, err := workspaceTransferDir(opts.PeerURL, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fetch", "checkpoint.json"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := workspaceTransferGC(time.Now().Add(time.Hour), false)
	if err == nil || result.Failed != 1 || result.Protected != 0 {
		t.Fatalf("GC hid corruption: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "origin.json")); err != nil {
		t.Fatalf("damaged state must remain: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "fetch", "checkpoint.json")); err != nil {
		t.Fatal(err)
	}
	result, err = workspaceTransferGC(time.Now().Add(time.Hour), false)
	if err == nil || result.Failed != 1 || result.Protected != 0 {
		t.Fatalf("GC hid missing checkpoint: %+v %v", result, err)
	}
}

func TestTransferInventorySkipsBusyRelationship(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	prep := prepareSnapshot(root, true, false)
	opts := RunOptions{PeerURL: "http://runner", Root: root}
	const id = "01M2280R0T4152A3BSV4C2976R"
	if err := recordWorkspaceOrigin(opts, id, prep.manifest); err != nil {
		t.Fatal(err)
	}
	dir, err := workspaceTransferDir(opts.PeerURL, id)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := lockWorkspaceTransfer(dir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := workspaceTransferStats(context.Background()); done <- err }()
	select {
	case err := <-done:
		unlock()
		if err == nil || !strings.Contains(err.Error(), "busy") {
			t.Fatalf("busy inventory: %v", err)
		}
	case <-time.After(time.Second):
		unlock()
		<-done
		t.Fatal("inventory blocked on active transfer")
	}
	unlock, err = lockWorkspaceTransfer(dir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := workspaceTransferGC(time.Now().Add(time.Hour), true)
	unlock()
	if err != nil || result.Protected != 1 {
		t.Fatalf("dry-run omitted active relationship: %+v %v", result, err)
	}
}

func TestTransferGCContinuesPastCorruptOrigin(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	prep := prepareSnapshot(root, true, false)
	opts := RunOptions{PeerURL: "http://runner", Root: root}
	var dirs []string
	for _, id := range []string{"01M2280R0T4152A3BSV4C2976R", "01M2280R0T4152A3BSV4C2976S"} {
		if err := recordWorkspaceOrigin(opts, id, prep.manifest); err != nil {
			t.Fatal(err)
		}
		dir, err := workspaceTransferDir(opts.PeerURL, id)
		if err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	if err := os.WriteFile(filepath.Join(dirs[0], "origin.json"), []byte("{bad"), 0600); err != nil {
		t.Fatal(err)
	}
	garbage := filepath.Join(dirs[1], ".source-abandoned")
	if err := os.Mkdir(garbage, 0700); err != nil {
		t.Fatal(err)
	}
	result, err := workspaceTransferGC(time.Now().Add(time.Hour), false)
	if err == nil || result.Failed != 1 || result.Removed != 1 {
		t.Fatalf("partial GC: %+v %v", result, err)
	}
	if _, err := os.Stat(garbage); !os.IsNotExist(err) {
		t.Fatal("healthy relationship was skipped", err)
	}
	stats, err := workspaceTransferStats(context.Background())
	if err == nil || stats.Items != 1 || stats.Bytes == 0 {
		t.Fatalf("partial inventory: %+v %v", stats, err)
	}
}

func TestTransferObserversDoNotReportEachOtherBusy(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	prep := prepareSnapshot(root, true, false)
	if err := recordWorkspaceOrigin(RunOptions{PeerURL: "http://runner", Root: root}, "01M2280R0T4152A3BSV4C2976R", prep.manifest); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(gc bool) {
			defer wg.Done()
			<-start
			for j := 0; j < 4; j++ {
				if gc {
					result, err := workspaceTransferGC(time.Now().Add(time.Hour), true)
					if err != nil || result.Protected != 0 {
						t.Errorf("read-only GC competed with observer: %+v %v", result, err)
						return
					}
				} else {
					result, err := workspaceTransferStats(t.Context())
					if err != nil || result.Items != 1 {
						t.Errorf("inventory competed with observer: %+v %v", result, err)
						return
					}
				}
			}
		}(i%2 == 0)
	}
	close(start)
	wg.Wait()
}
