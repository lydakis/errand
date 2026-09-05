package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/daemon"
)

func TestCacheFlagsAndInspection(t *testing.T) {
	writeClientConfig(t, "default_peer = 'test'\n[peers.test]\nurl = 'http://runner.invalid'\n[caches]\ncompiler = 'target'\n")
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		args []string
		path string
	}{
		{nil, "target"}, {[]string{"--cache", "compiler=out"}, "out"}, {[]string{"--no-caches"}, ""},
	} {
		var out, stderr bytes.Buffer
		if code := cmdConfigTo(append(tc.args, "--json"), &out, &stderr); code != 0 {
			t.Fatalf("config: %d %s", code, &stderr)
		}
		var got config.EffectiveRun
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if tc.path == "" {
			if len(got.Caches) != 0 {
				t.Fatal(got.Caches)
			}
		} else if len(got.Caches) != 1 || got.Caches[0].Path != tc.path {
			t.Fatal(got.Caches)
		}
	}
	for _, args := range [][]string{{"--cache", ""}, {"--cache", "a=out", "--no-caches"}, {"--cache", "a=../out"}} {
		if code := cmdRun(append(args, "--", "true")); code != 2 {
			t.Fatalf("accepted %v: %d", args, code)
		}
	}
}

func TestNamedCachesReuseWithoutSnapshotOrFetchContents(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true, NamedCacheMaxBytes: 1})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Close() })
			server := httptest.NewServer(d.Handler())
			t.Cleanup(server.Close)
			writeClientConfig(t, fmt.Sprintf("default_peer = 'test'\n[peers.test]\nurl = %q\n", server.URL))
			root := t.TempDir()
			t.Chdir(root)
			for name, content := range map[string]string{".errandignore": "", ".errand.toml": "[caches]\ncompiler = 'target'\n[artifacts]\npaths = ['target']\n", "target/local.txt": "not an input"} {
				if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for i, script := range []string{"test ! -e target/local.txt || exit 9; printf cache > target/state; printf report > report.txt", "test \"$(cat target/state)\" = cache || exit 9; printf failed > report.txt; exit 7"} {
				args := []string{"--no-apply"}
				if empty {
					args = append(args, "--no-snapshot")
				}
				code := cmdRun(append(args, "--", "/bin/sh", "-c", script))
				want := 0
				if i == 1 {
					want = 7
				}
				if code != want {
					t.Fatalf("run %d: %d", i, code)
				}
			}
			jobs, err := client.List(server.URL)
			if err != nil || len(jobs) != 2 {
				t.Fatalf("jobs: %v %v", jobs, err)
			}
			for _, job := range jobs {
				out := filepath.Join(t.TempDir(), "results")
				if code := cmdFetch([]string{"--output", out, "test/" + job.ID}); code != 0 {
					t.Fatalf("fetch: %d", code)
				}
				if _, err := os.Lstat(filepath.Join(out, "target")); !os.IsNotExist(err) {
					t.Fatalf("cache retained: %v", err)
				}
				if _, err := os.Stat(filepath.Join(out, "report.txt")); err != nil {
					t.Fatal(err)
				}
			}
			stats, err := client.StorageStats(server.URL)
			if err != nil || stats.NamedCaches == nil || stats.NamedCaches.Items != 1 || stats.NamedCaches.Bytes != 5 || stats.NamedCaches.Protected != 0 {
				t.Fatalf("stats: %+v %v", stats, err)
			}
			var stdout, stderr bytes.Buffer
			if code := cmdDfTo([]string{"--on", "test", "--json"}, &stdout, &stderr); code != 0 {
				t.Fatalf("df: %d %s", code, &stderr)
			}
			var rows []dfRow
			if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil || rows[0].NamedCaches == nil || rows[0].NamedCaches.Bytes != 5 {
				t.Fatalf("df: %s %v", &stdout, err)
			}
			for _, dry := range []bool{true, false} {
				result, err := client.CacheGC(server.URL, dry)
				if err != nil || result.RemovedCaches != 1 || result.ProtectedCaches != 0 {
					t.Fatalf("gc: %+v %v", result, err)
				}
			}
			stats, err = client.StorageStats(server.URL)
			if err != nil || stats.NamedCaches.Items != 0 {
				t.Fatalf("after gc: %+v %v", stats, err)
			}
			if got, err := os.ReadFile("target/local.txt"); err != nil || string(got) != "not an input" {
				t.Fatalf("local cache changed: %q %v", got, err)
			}
		})
	}
}
