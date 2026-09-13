package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func TestPushBatchUsesDeltaAndFailsClosedAfterRollback(t *testing.T) {
	var rollback atomic.Bool
	var legacyUploads, negotiations atomic.Int32
	root, destination, peer, ws := watchFixtureHandler(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/push") {
				legacyUploads.Add(1)
			}
			if strings.HasSuffix(r.URL.Path, "/push/diff") {
				negotiations.Add(1)
			}
			if strings.HasSuffix(r.URL.Path, "/push/delta-v1") && rollback.Load() {
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	opts := client.PushOptions{PeerURL: peer, Workspace: ws.Name, Root: root, Apply: true}
	if err := os.WriteFile(filepath.Join(root, "keep"), []byte(strings.Repeat("k", 128<<10)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PushChanges(opts); err != nil {
		t.Fatal(err)
	}
	negotiations.Store(0)
	var stats client.TransferStats
	opts.Stats = &stats
	if err := os.WriteFile(filepath.Join(root, "value"), []byte("edited\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PushChanges(opts); err != nil {
		t.Fatal(err)
	}
	if stats.TransferredBytes > 16<<10 || negotiations.Load() != 0 || legacyUploads.Load() != 0 {
		t.Fatalf("small batch failed delta path: %+v negotiations=%d legacy=%d", stats, negotiations.Load(), legacyUploads.Load())
	}
	rollback.Store(true)
	if err := os.Remove(filepath.Join(root, "value")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PushChanges(opts); err == nil {
		t.Fatal("rollback unexpectedly accepted delta")
	}
	if _, err := os.Stat(filepath.Join(destination, "keep")); err != nil {
		t.Fatalf("unrelated file lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "value")); err != nil {
		t.Fatalf("rollback applied deletion: %v", err)
	}
	if legacyUploads.Load() != 0 {
		t.Fatal("delta fell back to unsafe legacy endpoint")
	}
}

func TestPushBatchRebuildsOnlyTypedUnstagedCheckpointRejection(t *testing.T) {
	for _, typed := range []bool{true, false} {
		t.Run(map[bool]string{true: "checkpoint", false: "generic"}[typed], func(t *testing.T) {
			var rejected atomic.Bool
			var uploads, bases atomic.Int32
			root, destination, peer, ws := watchFixtureHandler(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/push/base") {
						bases.Add(1)
					}
					if strings.HasSuffix(r.URL.Path, "/push/delta-v1") {
						uploads.Add(1)
						if !rejected.Swap(true) {
							w.WriteHeader(http.StatusConflict)
							code := "other_conflict"
							if typed {
								code = proto.ErrorCodePushCheckpointChanged
							}
							json.NewEncoder(w).Encode(proto.APIError{Code: code, Error: "checkpoint changed"})
							return
						}
					}
					next.ServeHTTP(w, r)
				})
			})
			if err := os.WriteFile(filepath.Join(root, "value"), []byte("edited\n"), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := client.PushChanges(client.PushOptions{PeerURL: peer, Workspace: ws.Name, Root: root, Apply: true})
			if typed {
				if err != nil || uploads.Load() != 2 || bases.Load() != 2 {
					t.Fatalf("retry=%d base=%d err=%v", uploads.Load(), bases.Load(), err)
				}
				if body, err := os.ReadFile(filepath.Join(destination, "value")); err != nil || string(body) != "edited\n" {
					t.Fatalf("delivery=%q %v", body, err)
				}
			} else if err == nil || uploads.Load() != 1 {
				t.Fatalf("generic conflict retried: uploads=%d err=%v", uploads.Load(), err)
			}
		})
	}
}

// A stage-only watch and a one-shot apply share local pending state. Advancing
// the receiver checkpoint must not strand the watch on its earlier delta base.
func TestPushBatchRefreshesCheckpointAfterOneShotApply(t *testing.T) {
	root, destination, peer, ws := watchFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	step := 0
	opts := client.PushOptions{PeerURL: peer, Workspace: ws.Name, Root: root}
	err := client.WatchPush(ctx, opts, func(event client.PushWatchEvent) error {
		if event.State != "watching" {
			return nil
		}
		step++
		switch step {
		case 1:
			return os.WriteFile(filepath.Join(root, "value"), []byte("first\n"), 0600)
		case 2:
			apply := opts
			apply.Apply = true
			if _, err := client.PushChanges(apply); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(root, "value"), []byte("second\n"), 0600)
		case 3:
			apply := opts
			apply.Apply = true
			if _, err := client.PushChanges(apply); err != nil {
				return err
			}
			cancel()
		}
		return nil
	})
	if err != nil || step != 3 {
		t.Fatalf("step=%d err=%v", step, err)
	}
	if body, err := os.ReadFile(filepath.Join(destination, "value")); err != nil || string(body) != "second\n" {
		t.Fatalf("delivery=%q %v", body, err)
	}
}

func TestWatchRestoresRememberedSourceAfterAnotherPush(t *testing.T) {
	var requests atomic.Int32
	root, destination, peer, ws := watchFixtureHandler(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			next.ServeHTTP(w, r)
		})
	})
	name := filepath.Join(root, "value")
	if err := os.WriteFile(name, []byte("A\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := client.PushOptions{PeerURL: peer, Workspace: ws.Name, Root: root, Apply: true}
	step := 0
	var beforeNoop int32
	err := client.WatchPush(ctx, opts, func(event client.PushWatchEvent) error {
		if event.State != "watching" {
			return nil
		}
		step++
		if step == 1 {
			beforeNoop = requests.Load()
			return os.WriteFile(name, []byte("A\n"), 0600)
		}
		if step == 2 {
			if requests.Load() != beforeNoop {
				t.Error("unchanged watch performed network work")
			}
			if err := os.WriteFile(name, []byte("B\n"), 0600); err != nil {
				return err
			}
			if _, err := client.PushChanges(opts); err != nil {
				return err
			}
			return os.WriteFile(name, []byte("A\n"), 0600)
		}
		cancel()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "value"))
	if err != nil || string(got) != "A\n" {
		t.Fatalf("watch lost restoration: %q %v", got, err)
	}

}
