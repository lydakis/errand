package main

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/daemon"
)

func TestWorkspaceCreationDefaults(t *testing.T) {
	for _, scenario := range []string{"creation-cache-default", "creation-artifact-default"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			srv := httptest.NewServer(d.Handler())
			defer srv.Close()
			writeClientConfig(t, fmt.Sprintf("default_peer = 'test'\n[peers.test]\nurl = %q\n", srv.URL))
			t.Chdir(t.TempDir())
			if err := os.WriteFile(".errandignore", []byte("ignored/\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile("file", []byte("initial"), 0600); err != nil {
				t.Fatal(err)
			}
			name := "experiment"
			args := []string{"create"}
			switch scenario {
			case "creation-cache-default":
				args = append(args, "--cache", "compiler=target")
			case "creation-artifact-default":
				args = append(args, "--artifact", "ignored")
			}
			var out, stderr bytes.Buffer
			if code := cmdWorkspacesTo(append(args, name), &out, &stderr); code != 0 {
				t.Fatalf("create: %d %s", code, &stderr)
			}
			// Changing ambient config after creation cannot silently replace a
			// workspace's frozen bindings or its retained-output defaults.
			if err := os.WriteFile(".errand.toml", []byte("[caches]\nother = 'other-cache'\n[artifacts]\npaths = ['other-output']\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if code := cmdRun([]string{"--workspace", name, "--no-apply", "--", "/bin/sh", "-c", "mkdir -p ignored; echo output > ignored/out"}, nil); code != 0 {
				t.Fatalf("plain reuse failed: %d", code)
			}
			if scenario == "creation-artifact-default" {
				jobs, err := client.List(srv.URL)
				if err != nil || len(jobs) != 1 {
					t.Fatalf("jobs: %v %v", jobs, err)
				}
				job, err := client.GetJobDetails(srv.URL, jobs[0].ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(job.Spec.Selection.Artifacts) != 1 || job.Spec.Selection.Artifacts[0] != "ignored" {
					t.Fatalf("creation artifact default lost: %v", job.Spec.Selection.Artifacts)
				}
				if code := cmdRun([]string{"--workspace", name, "--no-artifacts", "--no-apply", "--", "true"}, nil); code != 0 {
					t.Fatalf("explicit artifact clear: %d", code)
				}
				jobs, err = client.List(srv.URL)
				if err != nil {
					t.Fatal(err)
				}
				var cleared bool
				for _, entry := range jobs {
					details, err := client.GetJobDetails(srv.URL, entry.ID)
					if err != nil {
						t.Fatal(err)
					}
					if len(details.Spec.Selection.Artifacts) == 0 {
						cleared = true
					}
				}
				if !cleared {
					t.Fatal("--no-artifacts did not clear retained-output defaults")
				}
			} else {
				if code := cmdRun([]string{"--workspace", name, "--no-caches", "--no-apply", "--", "true"}, nil); code == 0 {
					t.Fatal("explicit conflicting cache clear was silently ignored")
				}
			}
		})
	}
}
