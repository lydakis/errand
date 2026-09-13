package main

import (
	"context"
	"fmt"
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
)

// Attribute end-to-end time without adding profiling fields to transfer receipts.
func BenchmarkPushPhases(b *testing.B)  { benchmarkPushPhases(b, false) }
func BenchmarkWatchPhases(b *testing.B) { benchmarkPushPhases(b, true) }
func benchmarkPushPhases(b *testing.B, watch bool) {
	b.Setenv("XDG_STATE_HOME", b.TempDir())
	root := b.TempDir()
	os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0600)
	for i := range 10000 {
		os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%05d", i)), []byte(strings.Repeat("x", 1024)), 0600)
	}
	state := b.TempDir()
	d, err := daemon.New(daemon.Config{StateDir: state, InsecureNoAuth: true})
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	var stage, apply, negotiation atomic.Int64
	next := d.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		switch {
		case strings.HasSuffix(r.URL.Path, "/push/diff"):
			negotiation.Add(int64(time.Since(started)))
		case (strings.HasSuffix(r.URL.Path, "/push") || strings.HasSuffix(r.URL.Path, "/push/delta-v1")):
			stage.Add(int64(time.Since(started)))
		case strings.HasSuffix(r.URL.Path, "/apply"):
			apply.Add(int64(time.Since(started)))
		}
	}))
	defer server.Close()
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: server.URL, Root: root}, "bench")
	if err != nil {
		b.Fatal(err)
	}
	// Warm the workspace's retained source and staging paths first.
	opts := client.PushOptions{PeerURL: server.URL, Workspace: ws.Name, Root: root, Apply: true}
	if _, err := client.PushChanges(opts); err != nil {
		b.Fatal(err)
	}
	stage.Store(0)
	apply.Store(0)
	negotiation.Store(0)
	var elapsed time.Duration
	if watch {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		count := 0
		var started time.Time
		b.StopTimer()
		err := client.WatchPush(ctx, opts, func(event client.PushWatchEvent) error {
			if event.Result == nil {
				return nil
			}
			if event.Err != nil {
				return event.Err
			}
			if !started.IsZero() {
				elapsed += time.Since(started)
				count++
			}
			if count == b.N {
				cancel()
				return nil
			}
			if started.IsZero() {
				stage.Store(0)
				apply.Store(0)
				negotiation.Store(0)
				b.ResetTimer()
				b.StartTimer()
			}
			started = time.Now()
			return os.WriteFile(filepath.Join(root, "file-00000"), []byte(fmt.Sprintf("watch-%d", count)), 0600)
		})
		if err != nil {
			b.Fatal(err)
		}
	} else {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			os.WriteFile(filepath.Join(root, "file-00000"), []byte(fmt.Sprintf("edit-%d", i)), 0600)
			b.StartTimer()
			started := time.Now()
			if _, err := client.PushChanges(opts); err != nil {
				b.Fatal(err)
			}
			elapsed += time.Since(started)
		}

	}
	b.StopTimer()
	ms := float64(b.N) * float64(time.Millisecond)
	b.ReportMetric(float64(stage.Load())/ms, "stage-ms/op")
	b.ReportMetric(float64(apply.Load())/ms, "apply-ms/op")
	b.ReportMetric(float64(negotiation.Load())/ms, "negotiate-ms/op")
	b.ReportMetric(float64(int64(elapsed)-stage.Load()-apply.Load()-negotiation.Load())/ms, "client-ms/op")
}
