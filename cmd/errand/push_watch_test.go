package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/daemon"
	"github.com/lydakis/errand/internal/proto"
)

type watchReceipt struct {
	proto.PushResult
	transferReport
}

func TestPushWatchFlagsAndJSON(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(fmt.Sprintf("apply=%v", apply), func(t *testing.T) {
			root, destination, peer, ws := watchFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r, w := io.Pipe()
			defer r.Close()
			done := make(chan int, 1)
			args := []string{"--watch", "--profile", "dev", "--json"}
			if apply {
				args = append(args, "--apply")
			}
			args = append(args, "value")
			go func() { done <- cmdPushToContext(ctx, args, w, io.Discard); w.Close() }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("watch did not stop")
				}
			})
			receipts := make(chan watchReceipt, 10)
			go func() {
				defer close(receipts)
				decoder := json.NewDecoder(r)
				for {
					var row watchReceipt
					if decoder.Decode(&row) != nil {
						return
					}
					receipts <- row
				}
			}()
			next := func() watchReceipt {
				t.Helper()
				select {
				case row, ok := <-receipts:
					if !ok {
						t.Fatal("watch exited early")
					}
					return row
				case <-time.After(10 * time.Second):
					t.Fatal("missing watch receipt")
					return watchReceipt{}
				}
			}
			if row := next(); row.Status != "unchanged" {
				t.Fatalf("initial selected path: %+v", row)
			}
			for i := range 2 {
				body := fmt.Sprintf("edit-%d\n", i)
				if err := os.WriteFile(filepath.Join(root, "value"), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				row := next()
				want := "staged"
				if apply {
					want = "applied"
				}
				if row.Status != want || row.WorkspaceID != ws.ID || row.Transfer.ChangedPaths != 1 {
					t.Fatalf("receipt: %+v", row)
				}
				got, err := os.ReadFile(filepath.Join(destination, "value"))
				if err != nil {
					t.Fatal(err)
				}
				if !apply {
					body = "initial\n"
				}
				if string(got) != body {
					t.Fatalf("remote value: %q want %q", got, body)
				}
			}
			// Cache/build churn must not wake the transfer loop.
			for i := range 20 {
				os.WriteFile(filepath.Join(root, "ignored", "build"), []byte{byte(i)}, 0600)
			}
			select {
			case row := <-receipts:
				t.Fatalf("idle or ignored write pushed: %+v", row)
			case <-time.After(400 * time.Millisecond):
			}
			_ = peer
		})
	}
}

func TestPushWatchConflictsStopAfterNormalPushSemantics(t *testing.T) {
	for _, markers := range []bool{false, true} {
		t.Run(fmt.Sprintf("markers=%v", markers), func(t *testing.T) {
			root, destination, peer, ws := watchFixture(t)
			os.WriteFile(filepath.Join(root, "value"), []byte("local\n"), 0600)
			os.WriteFile(filepath.Join(destination, "value"), []byte("remote\n"), 0600)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var receipt *proto.PushResult
			err := client.WatchPush(ctx, client.PushOptions{PeerURL: peer, Workspace: ws.Name, Root: root, Apply: true, MaterializeConflicts: markers}, func(event client.PushWatchEvent) error {
				if event.Result != nil {
					receipt = event.Result
				}
				return nil
			})
			if err == nil || receipt == nil || len(receipt.Conflicts) != 1 {
				t.Fatalf("conflict not reported: %+v %v", receipt, err)
			}
			got, _ := os.ReadFile(filepath.Join(destination, "value"))
			if markers {
				if !strings.Contains(string(got), "<<<<<<<") {
					t.Fatalf("missing markers: %s", got)
				}
			} else if string(got) != "remote\n" {
				t.Fatalf("conflict changed remote: %s", got)
			}
		})
	}
}

func watchFixture(t *testing.T) (root, destination, peer string, ws proto.Workspace) {
	return watchFixtureHandler(t, func(h http.Handler) http.Handler { return h })
}

func watchFixtureHandler(t *testing.T, wrap func(http.Handler) http.Handler) (root, destination, peer string, ws proto.Workspace) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	state := t.TempDir()
	d, err := daemon.New(daemon.Config{StateDir: state, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	server := httptest.NewServer(wrap(d.Handler()))
	t.Cleanup(server.Close)
	root = t.TempDir()
	t.Chdir(root)
	writeClientConfig(t, fmt.Sprintf("[peers.test]\nurl = %q\n[profiles.dev.run]\npeer = 'test'\nworkspace = 'experiment'\n[profiles.dev.changes]\napply_on_success = true\n", server.URL))
	os.WriteFile(filepath.Join(root, ".errandignore"), []byte("ignored/\n"), 0600)
	os.Mkdir(filepath.Join(root, "ignored"), 0700)
	os.WriteFile(filepath.Join(root, "value"), []byte("initial\n"), 0600)
	ws, err = client.CreateWorkspace(client.RunOptions{PeerURL: server.URL, Root: root}, "experiment")
	if err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(state, "workspaces", ws.ID, "data"), server.URL, ws
}

func TestPushWatchRecoversLostApplyThenDeliversEditsMadeInFlight(t *testing.T) {
	var drop atomic.Bool
	var root string
	var attempted, recovered string
	r, destination, peer, ws := watchFixtureHandler(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/apply") {
				if drop.CompareAndSwap(false, true) {
					attempted = r.URL.Path
					response := httptest.NewRecorder()
					next.ServeHTTP(response, r)
					if response.Code != http.StatusOK {
						t.Errorf("initial apply: %d %s", response.Code, response.Body)
					}
					// A save arrives after the daemon applied the previous snapshot,
					// but before its receipt reaches the client.
					if err := os.WriteFile(filepath.Join(root, "value"), []byte("newer\n"), 0600); err != nil {
						t.Error(err)
					}
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}
				if recovered == "" {
					recovered = r.URL.Path
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	root = r
	os.WriteFile(filepath.Join(root, "value"), []byte("first\n"), 0600)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var sawRecovery, delivered bool
	err := client.WatchPush(ctx, client.PushOptions{PeerURL: peer, Workspace: ws.Name, Root: root, Apply: true}, func(event client.PushWatchEvent) error {
		if event.Result != nil && event.Err == nil {
			sawRecovery = sawRecovery || event.Result.Recovered
			got, _ := os.ReadFile(filepath.Join(destination, "value"))
			if !event.Result.Recovered && string(got) == "newer\n" {
				delivered = true
				cancel()
			}
		}
		return nil
	})
	if err != nil || !sawRecovery || !delivered || attempted != recovered {
		t.Fatalf("recovery=%v delivered=%v attempts=%q/%q err=%v", sawRecovery, delivered, attempted, recovered, err)
	}
}

func TestPushWatchStopsWhenSelectionChanges(t *testing.T) {
	root, _, peer, ws := watchFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	changed := false
	err := client.WatchPush(ctx, client.PushOptions{PeerURL: peer, Workspace: ws.Name, Root: root, Apply: true}, func(event client.PushWatchEvent) error {
		if event.State == "watching" && !changed {
			changed = true
			return os.WriteFile(filepath.Join(root, ".errandignore"), []byte("ignored/\nvalue\n"), 0600)
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "selection policy differs") {
		t.Fatalf("policy change: %v", err)
	}
}

func TestPushWatchDoesNotFollowRecreatedWorkspaceName(t *testing.T) {
	root, _, peer, ws := watchFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	recreated := false
	err := client.WatchPush(ctx, client.PushOptions{PeerURL: peer, Workspace: ws.Name, Root: root, Apply: true}, func(event client.PushWatchEvent) error {
		if event.State == "watching" && !recreated {
			recreated = true
			if _, err := client.RemoveWorkspace(peer, ws.Name); err != nil {
				return err
			}
			if _, err := client.CreateWorkspace(client.RunOptions{PeerURL: peer, Root: root}, ws.Name); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(root, "value"), []byte("must not arrive\n"), 0600)
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("watch followed new workspace: %v", err)
	}
}
