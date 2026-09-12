package main

import (
	"bytes"
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

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/daemon"
	"github.com/lydakis/errand/internal/proto"
)

func TestTransferReportingRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	var uploaded, downloaded atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/push") {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			r.Body.Close()
			uploaded.Store(int64(len(body)))
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		if strings.HasSuffix(r.URL.Path, "/changes") {
			recorded := httptest.NewRecorder()
			d.Handler().ServeHTTP(recorded, r)
			downloaded.Store(int64(recorded.Body.Len()))
			for key, values := range recorded.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(recorded.Body.Bytes())
			return
		}
		d.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	writeClientConfig(t, fmt.Sprintf("default_peer='test'\n[peers.test]\nurl=%q\n", server.URL))
	root := t.TempDir()
	t.Chdir(root)
	if err := os.WriteFile(".errandignore", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("value", []byte("one\ntwo\nthree\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateWorkspace(client.RunOptions{PeerURL: server.URL, Root: root}, "dev"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("value", []byte("one\ntwo\nlocal\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	var report struct {
		Status   string               `json:"status"`
		Transfer client.TransferStats `json:"transfer"`
	}
	if code := cmdPushTo([]string{"--workspace", "dev"}, &out, &stderr); code != 0 {
		t.Fatalf("push: %d %s", code, &stderr)
	}
	if !proto.ValidULID(strings.TrimSpace(out.String())) || !strings.Contains(stderr.String(), "push: staged 1 changed path") || !strings.Contains(stderr.String(), "transferred in") {
		t.Fatalf("stage output: %s %s", &out, &stderr)
	}
	out.Reset()
	stderr.Reset()
	if code := cmdPushTo([]string{"--workspace", "dev", "--json"}, &out, &stderr); code != 0 {
		t.Fatalf("push JSON: %d %s", code, &stderr)
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if uploaded.Load() == 0 || report.Transfer.TransferredBytes != uploaded.Load() {
		t.Fatalf("upload body bytes: %+v want %d", report, uploaded.Load())
	}
	out.Reset()
	stderr.Reset()
	if code := cmdPushTo([]string{"--workspace", "dev", "--apply", "--json"}, &out, &stderr); code != 0 {
		t.Fatalf("push apply: %d %s", code, &stderr)
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != "applied" || report.Transfer.ChangedPaths != 1 || report.Transfer.TransferredBytes != 0 {
		t.Fatalf("staged apply stats: %+v", report)
	}
	if code := client.Run(client.RunOptions{PeerURL: server.URL, Root: root, Workspace: "dev", Argv: []string{"sh", "-c", "printf 'remote\\ntwo\\nlocal\\n' > value"}, Stdout: io.Discard, Stderr: &stderr}); code != 0 {
		t.Fatalf("run: %d %s", code, &stderr)
	}
	jobs, err := client.List(server.URL)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs: %v %v", jobs, err)
	}
	handle := "test/" + jobs[0].ID
	out.Reset()
	stderr.Reset()
	if code := cmdFetchTo([]string{"--json", handle}, &out, &stderr); code != 0 {
		t.Fatalf("fetch JSON: %d %s", code, &stderr)
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if downloaded.Load() == 0 || report.Transfer.TransferredBytes != downloaded.Load() {
		t.Fatalf("download body bytes: %+v want %d", report, downloaded.Load())
	}
	out.Reset()
	stderr.Reset()
	if code := cmdFetchTo([]string{handle}, &out, &stderr); code != 0 {
		t.Fatalf("fetch: %d %s", code, &stderr)
	}
	if _, err := os.Stat(strings.TrimSpace(out.String())); err != nil {
		t.Fatal("fetch stdout lost its staged path", err)
	}
	if !strings.Contains(stderr.String(), "fetch: staged 1 changed path") || !strings.Contains(stderr.String(), "transferred in") {
		t.Fatalf("fetch output: %s", &stderr)
	}
	out.Reset()
	stderr.Reset()
	if code := cmdFetchTo([]string{"--apply", "--json", handle}, &out, &stderr); code != 0 {
		t.Fatalf("fetch apply: %d %s", code, &stderr)
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != "applied" || report.Transfer.ChangedPaths != 1 || report.Transfer.TransferredBytes != 0 {
		t.Fatalf("cached fetch stats: %+v", report)
	}
	if content, err := os.ReadFile("value"); err != nil || string(content) != "remote\ntwo\nlocal\n" {
		t.Fatalf("fetch not applied: %q %v", content, err)
	}
}

func TestRunBindingOutputIsCompactUnlessVerbose(t *testing.T) {
	e := config.EffectiveRun{Artifacts: []string{"out"}}
	for i := 0; i < 26; i++ {
		e.Caches = append(e.Caches, proto.CacheBinding{Name: fmt.Sprintf("cache%d", i), Path: fmt.Sprintf("packages/%d/node_modules", i)})
	}
	var out bytes.Buffer
	printRunBindings(&out, e, false)
	if strings.Count(out.String(), "\n") != 1 || !strings.Contains(out.String(), "26 caches") || strings.Contains(out.String(), "node_modules") {
		t.Fatalf("noisy summary: %s", &out)
	}
	out.Reset()
	printRunBindings(&out, e, true)
	if !strings.Contains(out.String(), "packages/25/node_modules") || !strings.Contains(out.String(), "out") {
		t.Fatal("verbose output lost binding details")
	}
}

func TestFetchReportingSelectionAndConflict(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprint(persistent), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Close() })
			server := httptest.NewServer(d.Handler())
			t.Cleanup(server.Close)
			writeClientConfig(t, fmt.Sprintf("default_peer='test'\n[peers.test]\nurl=%q\n", server.URL))
			root := t.TempDir()
			t.Chdir(root)
			for path, body := range map[string]string{".errandignore": "", "conflict": "base\n", "deleted": "remove\n"} {
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			opts := client.RunOptions{PeerURL: server.URL, Root: root, Stdout: io.Discard, Stderr: io.Discard, Argv: []string{"sh", "-c", "printf 'remote\\n' > conflict; rm deleted; mkdir result; printf artifact > result/file"}}
			if persistent {
				if _, err := client.CreateWorkspace(opts, "dev"); err != nil {
					t.Fatal(err)
				}
				opts.Workspace = "dev"
			}
			if code := client.Run(opts); code != 0 {
				t.Fatalf("run: %d", code)
			}
			jobs, err := client.List(server.URL)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("jobs: %v %v", jobs, err)
			}
			handle := "test/" + jobs[0].ID
			var out, stderr bytes.Buffer
			var report struct {
				Status       string               `json:"status"`
				Transfer     client.TransferStats `json:"transfer"`
				Conflicts    []string             `json:"conflicts"`
				Materialized bool                 `json:"materialized"`
			}
			for _, extra := range [][]string{nil, {"--output", filepath.Join(t.TempDir(), "export")}} {
				out.Reset()
				stderr.Reset()
				args := append([]string{"--json"}, extra...)
				if code := cmdFetchTo(append(args, handle, "result"), &out, &stderr); code != 0 {
					t.Fatalf("fetch selection: %d %s", code, &stderr)
				}
				if err := json.Unmarshal(out.Bytes(), &report); err != nil {
					t.Fatal(err)
				}
				if report.Transfer.ChangedPaths != 1 {
					t.Fatalf("selection counted entire bundle: %+v", report)
				}
				if extra != nil && (report.Status != "exported" || report.Transfer.TransferredBytes != 0) {
					t.Fatalf("export stats: %+v", report)
				}
			}
			if err := os.WriteFile("conflict", []byte("local\n"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, materialize := range []bool{false, true} {
				out.Reset()
				stderr.Reset()
				args := []string{"--json", "--apply"}
				if materialize {
					args = append(args, "--conflicts")
				}
				if code := cmdFetchTo(append(args, handle), &out, &stderr); code != client.ExitTransaction {
					t.Fatalf("conflict: %d %s", code, &stderr)
				}
				if err := json.Unmarshal(out.Bytes(), &report); err != nil {
					t.Fatal(err)
				}
				want := 0
				if materialize {
					want = 2
				}
				if report.Status != "conflicted" || len(report.Conflicts) != 1 || report.Materialized != materialize || report.Transfer.ChangedPaths != want || report.Transfer.TransferredBytes != 0 {
					t.Fatalf("conflict stats: %+v", report)
				}
				if strings.Contains(stderr.String(), "file changes applied;") {
					t.Fatal("conflict printed success")
				}
			}
		})
	}
}
