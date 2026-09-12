package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/daemon"
	"github.com/lydakis/errand/internal/proto"
)

func TestArtifactFlagsAndInspection(t *testing.T) {
	writeClientConfig(t, "default_peer = 'test'\n[peers.test]\nurl = 'http://runner.invalid'\n[artifacts]\npaths = ['configured']\n")
	t.Chdir(t.TempDir())
	for _, tc := range []struct{ args, want []string }{
		{nil, []string{"configured"}},
		{[]string{"--artifact", "out", "--artifact", "report.txt"}, []string{"out", "report.txt"}},
		{[]string{"--no-artifacts"}, []string{}},
	} {
		var out, stderr bytes.Buffer
		if code := cmdConfigTo(append(tc.args, "--json"), &out, &stderr); code != 0 {
			t.Fatalf("config: %d %s", code, &stderr)
		}
		var got config.EffectiveRun
		if err := json.Unmarshal(out.Bytes(), &got); err != nil || !slices.Equal(got.Artifacts, tc.want) {
			t.Fatalf("artifacts: %+v %v", got, err)
		}
	}
	for _, args := range [][]string{{"--artifact", ""}, {"--artifact", "out", "--no-artifacts"}, {"--artifact", "../out"}} {
		if code := cmdRun(append(args, "--", "true")); code != 2 {
			t.Fatalf("accepted flags %v: %d", args, code)
		}
	}
}

func TestArtifactsRunFetchAndApply(t *testing.T) {
	for _, noSnapshot := range []bool{false, true} {
		t.Run(fmt.Sprint(noSnapshot), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			state := t.TempDir()
			d, err := daemon.New(daemon.Config{StateDir: state, InsecureNoAuth: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Close() })
			server := httptest.NewServer(d.Handler())
			t.Cleanup(server.Close)
			writeClientConfig(t, fmt.Sprintf("default_peer = 'test'\n[peers.test]\nurl = %q\n", server.URL))
			root := t.TempDir()
			t.Chdir(root)
			for name, content := range map[string]string{
				".errandignore":     "ignored/\n",
				".errand.toml":      "[artifacts]\npaths = ['ignored/reports']\n",
				"ignored/local.txt": "local input stays here",
			} {
				if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"--no-apply"}
			if noSnapshot {
				args = append(args, "--no-snapshot")
			}
			args = append(args, "--", "/bin/sh", "-c", "test ! -e ignored/local.txt || exit 9; mkdir -p ignored/reports; printf report > ignored/reports/result.txt; exit 7")
			if code := cmdRun(args); code != 7 {
				t.Fatalf("run: %d", code)
			}
			jobs, err := client.List(server.URL)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("jobs: %+v %v", jobs, err)
			}
			id := jobs[0].ID
			output := filepath.Join(t.TempDir(), "output")
			if code := cmdFetch([]string{"-o", output, "test/" + id}); code != 0 {
				t.Fatalf("export: %d", code)
			}
			if got, err := os.ReadFile(filepath.Join(output, "ignored/reports/result.txt")); err != nil || string(got) != "report" {
				t.Fatalf("export: %q %v", got, err)
			}
			if _, err := os.Stat("ignored/reports/result.txt"); !os.IsNotExist(err) {
				t.Fatal("export applied artifact")
			}
			// The ignored local directory was never submitted, so applying a
			// different added directory must retain the normal add/add refusal.
			if code := cmdFetch([]string{"--apply", "test/" + id}); code != client.ExitTransaction {
				t.Fatalf("overlapping unsubmitted directory: %d", code)
			}
			if err := os.Rename("ignored", "saved-local"); err != nil {
				t.Fatal(err)
			}
			if code := cmdFetch([]string{"--apply", "test/" + id}); code != 0 {
				t.Fatalf("apply: %d", code)
			}
			if got, err := os.ReadFile("ignored/reports/result.txt"); err != nil || string(got) != "report" {
				t.Fatalf("apply: %q %v", got, err)
			}
			if got, err := os.ReadFile("saved-local/local.txt"); err != nil || string(got) != "local input stays here" {
				t.Fatalf("local input changed: %q %v", got, err)
			}
			raw, err := os.ReadFile(filepath.Join(state, "jobs", id, "spec.json"))
			if err != nil {
				t.Fatal(err)
			}
			var receipt proto.ReceiptSpec
			if err := json.Unmarshal(raw, &receipt); err != nil || !slices.Equal(receipt.Selection.Artifacts, []string{"ignored/reports"}) {
				t.Fatalf("receipt artifacts: %+v %v", receipt.Selection, err)
			}
		})
	}
}
