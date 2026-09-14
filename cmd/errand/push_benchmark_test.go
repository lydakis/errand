package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

type watchWorkload struct {
	count       int
	nested, git bool
	edit        string
}

func BenchmarkWatchWorkloads(b *testing.B) {
	for _, scenario := range []struct {
		name     string
		workload watchWorkload
	}{
		{"small", watchWorkload{count: 1000}},
		{"git-atomic", watchWorkload{count: 10000, git: true, edit: "atomic"}},
		{"nested-atomic", watchWorkload{count: 10000, nested: true, edit: "atomic"}},
		{"structural", watchWorkload{count: 10000, edit: "rename"}},
	} {
		b.Run(scenario.name, func(b *testing.B) { benchmarkPushWorkload(b, true, scenario.workload) })
	}
}

func benchmarkPushPhases(b *testing.B, watch bool) {
	benchmarkPushWorkload(b, watch, watchWorkload{count: 10000})
}

func benchmarkPushWorkload(b *testing.B, watch bool, workload watchWorkload) {
	defer benchmarkCleanupPhase(b)()
	fixtureDone := benchmarkPhase(b, "fixture")
	b.Setenv("XDG_STATE_HOME", b.TempDir())
	root := b.TempDir()
	if !workload.git {
		if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0600); err != nil {
			b.Fatal(err)
		}
	}
	nameFor := func(i int) string {
		name := fmt.Sprintf("file-%05d", i)
		if workload.nested {
			name = fmt.Sprintf("packages/pkg-%03d/src/%s", i/100, name)
		}
		return name
	}
	for i := range workload.count {
		name := filepath.Join(root, nameFor(i))
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(strings.Repeat("x", 1024)), 0600); err != nil {
			b.Fatal(err)
		}
	}
	if workload.git {
		for _, args := range [][]string{{"init", "-q"}, {"add", "."}} {
			cmd := exec.Command("git", args...)
			cmd.Dir = root
			if out, err := cmd.CombinedOutput(); err != nil {
				b.Fatalf("git fixture: %v %s", err, out)
			}
		}
	}
	state := b.TempDir()
	fixtureDone()
	daemonDone := benchmarkPhase(b, "daemon-start")
	d, err := daemon.New(daemon.Config{StateDir: state, InsecureNoAuth: true})
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		done := benchmarkPhase(b, "daemon-close")
		d.Close()
		done()
	}()
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
	daemonDone()
	createDone := benchmarkPhase(b, "workspace-create")
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: server.URL, Root: root}, "bench")
	createDone()
	if err != nil {
		b.Fatal(err)
	}
	// Warm the workspace's retained source and staging paths first.
	opts := client.PushOptions{PeerURL: server.URL, Workspace: ws.Name, Root: root, Apply: true}
	warmDone := benchmarkPhase(b, "warm-push")
	if _, err := client.PushChanges(opts); err != nil {
		b.Fatal(err)
	}
	warmDone()
	stage.Store(0)
	apply.Store(0)
	negotiation.Store(0)
	var elapsed time.Duration
	currentName, expectedBody := nameFor(0), ""
	verify := func() {
		if expectedBody == "" {
			return
		}
		actual, err := os.ReadFile(filepath.Join(state, "workspaces", ws.ID, "data", currentName))
		if err != nil || string(actual) != expectedBody {
			b.Fatalf("delivery %s: %q %v", currentName, actual, err)
		}
	}
	edit := func(count int) error {
		if workload.edit == "rename" {
			next := nameFor(0)
			if count%2 == 0 {
				next += "-moved"
			}
			if err := os.Rename(filepath.Join(root, currentName), filepath.Join(root, next)); err != nil {
				return err
			}
			currentName, expectedBody = next, strings.Repeat("x", 1024)
			return nil
		}
		expectedBody = fmt.Sprintf("edit-%d", count)
		target := filepath.Join(root, currentName)
		if workload.edit == "atomic" {
			tmp := target + ".save"
			if err := os.WriteFile(tmp, []byte(expectedBody), 0600); err != nil {
				return err
			}
			return os.Rename(tmp, target)
		}
		return os.WriteFile(target, []byte(expectedBody), 0600)
	}
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
				verify()
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
			return edit(count)
		})
		if err != nil {
			b.Fatal(err)
		}
	} else {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			if err := edit(i); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			started := time.Now()
			if _, err := client.PushChanges(opts); err != nil {
				b.Fatal(err)
			}
			elapsed += time.Since(started)
			b.StopTimer()
			verify()
			b.StartTimer()
		}

	}
	b.StopTimer()
	ms := float64(b.N) * float64(time.Millisecond)
	b.ReportMetric(float64(stage.Load())/ms, "stage-ms/op")
	b.ReportMetric(float64(apply.Load())/ms, "apply-ms/op")
	b.ReportMetric(float64(negotiation.Load())/ms, "negotiate-ms/op")
	b.ReportMetric(float64(int64(elapsed)-stage.Load()-apply.Load()-negotiation.Load())/ms, "client-ms/op")
}
