package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/daemon"
)

func BenchmarkWorkspaceCreationAndSubmission(b *testing.B) {
	for _, kind := range []string{"workspace-create", "ephemeral-job"} {
		b.Run(kind, func(b *testing.B) {
			b.Setenv("XDG_STATE_HOME", b.TempDir())
			root := b.TempDir()
			if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0600); err != nil {
				b.Fatal(err)
			}
			for i := 0; i < 1000; i++ {
				if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%04d", i)), []byte(fmt.Sprintf("%04d%s", i, strings.Repeat("x", 1020))), 0600); err != nil {
					b.Fatal(err)
				}
			}
			state := b.TempDir()
			d, err := daemon.New(daemon.Config{StateDir: state, InsecureNoAuth: true})
			if err != nil {
				b.Fatal(err)
			}
			defer d.Close()
			server := httptest.NewServer(d.Handler())
			defer server.Close()
			b.ResetTimer()
			b.StopTimer()
			for i := 0; i < b.N; i++ {
				var out bytes.Buffer
				b.StartTimer()
				start := time.Now()
				if kind == "workspace-create" {
					ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: server.URL, Root: root}, fmt.Sprintf("create-%d", i))
					duration := time.Since(start)
					b.StopTimer()
					if err != nil {
						b.Fatal(err)
					}
					body, err := os.ReadFile(filepath.Join(state, "workspaces", ws.ID, "data", "file-0000"))
					if err != nil || len(body) != 1024 {
						b.Fatalf("created source: %v", err)
					}
					row, _ := json.Marshal(map[string]any{"mode": kind, "sample": i, "seconds": duration.Seconds()})
					fmt.Printf("EVALUATION %s\n", row)
				} else {
					code := client.Run(client.RunOptions{PeerURL: server.URL, Root: root, Argv: []string{"sh", "-c", "test -s file-0000 && test -s file-0999"}, Stdout: &out, Stderr: &out})
					duration := time.Since(start)
					b.StopTimer()
					if code != 0 {
						b.Fatalf("job: %d %s", code, out.String())
					}
					row, _ := json.Marshal(map[string]any{"mode": kind, "sample": i, "seconds": duration.Seconds()})
					fmt.Printf("EVALUATION %s\n", row)
				}
			}
		})
	}
}

// BenchmarkFetchCompletion times download plus local apply of a completed result.
// Remote job execution and result capture are explicitly outside the timer.
func BenchmarkFetchCompletion(b *testing.B) {
	for _, persistent := range []bool{false, true} {
		b.Run(fmt.Sprintf("persistent=%t", persistent), func(b *testing.B) {
			b.Setenv("XDG_STATE_HOME", b.TempDir())
			root := b.TempDir()
			write := func(name, body string) {
				if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
					b.Fatal(err)
				}
			}
			write(".errandignore", "")
			for i := 0; i < 1000; i++ {
				write(fmt.Sprintf("file-%04d", i), fmt.Sprintf("%04d%s", i, strings.Repeat("x", 1020)))
			}
			write("edit.txt", "initial\n")
			d, err := daemon.New(daemon.Config{StateDir: b.TempDir(), InsecureNoAuth: true})
			if err != nil {
				b.Fatal(err)
			}
			defer d.Close()
			server := httptest.NewServer(d.Handler())
			defer server.Close()
			workspace := ""
			if persistent {
				ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: server.URL, Root: root}, "eval")
				if err != nil {
					b.Fatal(err)
				}
				workspace = ws.Name
			}
			var elapsed time.Duration
			b.ResetTimer()
			b.StopTimer()
			for i := 0; i < b.N; i++ {
				body := fmt.Sprintf("fetch-%d", i)
				var out bytes.Buffer
				code := client.Run(client.RunOptions{PeerURL: server.URL, Root: root, Workspace: workspace, Argv: []string{"sh", "-c", "printf '" + body + "' > edit.txt"}, Stdout: &out, Stderr: &out})
				if code != 0 {
					b.Fatalf("job: %d %s", code, out.String())
				}
				jobs, err := client.List(server.URL)
				if err != nil || len(jobs) == 0 {
					b.Fatalf("jobs: %v", err)
				}
				var stats client.TransferStats
				b.StartTimer()
				start := time.Now()
				_, err = client.FetchChanges(client.ChangeFetchOptions{PeerURL: server.URL, JobID: jobs[0].ID, Apply: true, CallerDir: root, Stats: &stats})
				duration := time.Since(start)
				elapsed += duration
				b.StopTimer()
				if err != nil {
					b.Fatal(err)
				}
				actual, err := os.ReadFile(filepath.Join(root, "edit.txt"))
				if err != nil || string(actual) != body {
					b.Fatalf("contents: %q %v", actual, err)
				}
				row, _ := json.Marshal(map[string]any{"mode": "fetch", "persistent": persistent, "sample": i, "seconds": duration.Seconds(), "transferred_bytes": stats.TransferredBytes})
				fmt.Printf("EVALUATION %s\n", row)
			}
			b.ReportMetric(float64(elapsed)/float64(b.N)/float64(time.Millisecond), "fetch-ms/op")
		})
	}
}
