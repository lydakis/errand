package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/daemon"
)

func TestVersionWarningDoesNotGateRunAttachOrFetch(t *testing.T) {
	bin := buildErrand(t)
	for _, daemonVersion := range []string{"older", version, ""} {
		t.Run(fmt.Sprintf("version=%s", daemonVersion), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true, Version: daemonVersion})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			handler := d.Handler()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v0/info" && daemonVersion == "" {
					http.NotFound(w, r)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			root := t.TempDir()
			run := func(wantExit int, args ...string) (string, string) {
				t.Helper()
				cmd := exec.Command(bin, args...)
				cmd.Dir = root
				var out, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &out, &stderr
				err := cmd.Run()
				code := 0
				if err != nil {
					if e, ok := err.(*exec.ExitError); ok {
						code = e.ExitCode()
					} else {
						t.Fatal(err)
					}
				}
				if code != wantExit {
					t.Fatalf("exit %d want %d: %s", code, wantExit, &stderr)
				}
				count := strings.Count(stderr.String(), "errand: warning: CLI ")
				want := 0
				if daemonVersion == "older" {
					want = 1
				}
				if count != want {
					t.Fatalf("warning count %d want %d: %s", count, want, &stderr)
				}
				if want == 1 && (!strings.Contains(stderr.String(), "runner older") || !strings.Contains(stderr.String(), "installed version")) {
					t.Fatalf("missing version distinction: %s", &stderr)
				}
				return out.String(), stderr.String()
			}
			out, logs := run(7, "--url", server.URL, "--no-snapshot", "--no-apply", "--no-caches", "--artifact", "result.txt", "--", "/bin/sh", "-c", "printf retained > result.txt; printf output; exit 7")
			if out != "output" {
				t.Fatalf("stdout polluted: %q", out)
			}
			if daemonVersion == "older" && strings.Index(logs, "warning:") > strings.Index(logs, "errand: job ") {
				t.Fatal("warning arrived after admission")
			}
			jobs, err := client.List(server.URL)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("jobs: %v %v", jobs, err)
			}
			handle := server.URL + "/" + jobs[0].ID
			out, _ = run(7, "attach", handle)
			if out != "output" {
				t.Fatalf("attach stdout: %q", out)
			}
			dest := filepath.Join(root, "export")
			run(0, "fetch", "--output", dest, handle)
			got, err := os.ReadFile(filepath.Join(dest, "result.txt"))
			if err != nil || string(got) != "retained" {
				t.Fatalf("fetch: %q %v", got, err)
			}
		})
	}
}
