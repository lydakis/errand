package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
)

// A writer may change the source while it is being frozen. Resampling must
// wait for a stable snapshot and converge instead of exhausting retries just
// because native notifications arrive faster than the resampling delay.
func TestPushWatchConvergesAfterActiveWriter(t *testing.T) {
	root, destination, peer, ws := watchFixture(t)
	for i := range 100 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("background-%04d", i)), []byte("background\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ready := make(chan struct{})
	writing := make(chan error, 1)
	go func() {
		select {
		case <-ready:
		case <-ctx.Done():
			writing <- ctx.Err()
			return
		}
		for i := range 60 {
			if err := os.WriteFile(filepath.Join(root, "value"), []byte(fmt.Sprintf("burst-%d\n", i)), 0600); err != nil {
				writing <- err
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		writing <- nil
	}()
	started, delivered := false, false
	err := client.WatchPush(ctx, client.PushOptions{PeerURL: peer, Workspace: ws.Name, Root: root, Apply: true}, func(event client.PushWatchEvent) error {
		if event.State == "watching" && !started {
			started = true
			close(ready)
		}
		if event.Result != nil && event.Err == nil {
			body, _ := os.ReadFile(filepath.Join(destination, "value"))
			if string(body) == "burst-59\n" {
				delivered = true
				cancel()
			}
		}
		return nil
	})
	cancel()
	if writeErr := <-writing; writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil || !delivered {
		t.Fatalf("burst delivery=%v error=%v", delivered, err)
	}
}
