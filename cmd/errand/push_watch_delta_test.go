package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
)

func TestPushWatchStructuralEditsAndOldRunnerFallback(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			var negotiations atomic.Int32
			root, destination, peer, ws := watchFixtureHandler(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if legacy && strings.HasSuffix(r.URL.Path, "/push/base") {
						http.NotFound(w, r)
						return
					}
					if strings.HasSuffix(r.URL.Path, "/push/diff") {
						negotiations.Add(1)
					}
					next.ServeHTTP(w, r)
				})
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			step := 0
			err := client.WatchPush(ctx, client.PushOptions{PeerURL: peer, Workspace: ws.Name, Root: root, Apply: true}, func(event client.PushWatchEvent) error {
				if event.State != "watching" {
					return nil
				}
				switch step {
				case 0:
					if err := os.MkdirAll(filepath.Join(root, "new", "nested"), 0700); err != nil {
						return err
					}
					if err := os.WriteFile(filepath.Join(root, "new", "nested", "file"), []byte("created\n"), 0600); err != nil {
						return err
					}
				case 1:
					if body, err := os.ReadFile(filepath.Join(destination, "new", "nested", "file")); err != nil || string(body) != "created\n" {
						return fmt.Errorf("new directory not delivered: %q %v", body, err)
					}
					if err := os.Rename(filepath.Join(root, "new"), filepath.Join(root, "renamed")); err != nil {
						return err
					}
					if err := os.Remove(filepath.Join(root, "value")); err != nil {
						return err
					}
					if err := os.Symlink("renamed/nested/file", filepath.Join(root, "value")); err != nil {
						return err
					}
				case 2:
					if _, err := os.Stat(filepath.Join(destination, "new")); !os.IsNotExist(err) {
						return fmt.Errorf("renamed directory remains: %v", err)
					}
					if link, err := os.Readlink(filepath.Join(destination, "value")); err != nil || link != "renamed/nested/file" {
						return fmt.Errorf("file replacement not delivered: %q %v", link, err)
					}
					cancel()
				}
				step++
				return nil
			})
			if err != nil || step != 3 {
				t.Fatalf("step=%d: %v", step, err)
			}
			if !legacy && negotiations.Load() != 0 {
				t.Fatal("small deltas incurred redundant cache negotiations")
			}
		})
	}
}

func TestPushWatchCancelDrainsUnchangedSelectedPath(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	root, _, peer, ws := watchFixtureHandler(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/push") || strings.HasSuffix(r.URL.Path, "/push/delta-v1") {
				entered <- struct{}{}
				<-release
			}
			next.ServeHTTP(w, r)
		})
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- client.WatchPush(ctx, client.PushOptions{PeerURL: peer, Workspace: ws.Name, Root: root, Apply: true, Path: "value"}, func(client.PushWatchEvent) error { return nil })
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("initial push did not start")
	}
	cancel()
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("canceling an unchanged push failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watch did not drain")
	}
}
