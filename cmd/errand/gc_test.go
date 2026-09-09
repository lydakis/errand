package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
)

func TestGCMultiplePeersRequiresSelectionBeforeAnyWork(t *testing.T) {
	for _, test := range []struct{ name, personal, daemon string }{
		{"two remote runners", "default_peer='cabal'\n[peers.cabal]\nurl=%[1]q\n[peers.mini]\nurl=%[1]q\n", ""},
		{"local default and remote", "default_peer='local'\n[peers.cabal]\nurl=%q\n", ""},
		{"installed local and remote", "default_peer='cabal'\n[peers.cabal]\nurl=%q\n", "transport='local'\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				http.Error(w, "must not contact a default runner", 500)
			}))
			defer server.Close()
			writeClientConfig(t, fmt.Sprintf(test.personal, server.URL))
			if test.daemon != "" {
				path, err := config.DaemonPath()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(test.daemon), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Invalid local state proves gc all stops before local collection as well.
			t.Setenv("XDG_STATE_HOME", "relative-path")
			for _, target := range []string{"cache", "jobs", "all"} {
				for _, dryRun := range []bool{false, true} {
					args := []string{target}
					if target != "cache" {
						args = append(args, "--older-than", "30d")
					}
					if dryRun {
						args = append(args, "--dry-run")
					}
					var stdout, stderr bytes.Buffer
					code := cmdGCTo(args, &stdout, &stderr)
					if code == 0 || calls != 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "multiple runners configured; select one with --on PEER or --url URL") {
						t.Fatalf("gc %v = %d, calls=%d, stdout=%q stderr=%q", args, code, calls, stdout.String(), stderr.String())
					}
				}
			}
		})
	}
}

func TestGCSoleImplicitLocalRunner(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(fmt.Sprintf("installed=%v", installed), func(t *testing.T) {
			personal := "default_peer='local'\n"
			if installed {
				personal = ""
			}
			writeClientConfig(t, personal)
			if installed {
				path, err := config.DaemonPath()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("transport='local'\nsocket='/tmp/errand-gc-test.sock'\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			url, label, err := resolveGCPeerTarget("", "")
			if err != nil || label != "local" || !strings.HasPrefix(url, "unix://") {
				t.Fatalf("sole local runner: label=%q url=%q err=%v", label, url, err)
			}
		})
	}
}

func TestGCPeerSelection(t *testing.T) {
	for _, test := range []struct {
		name, config string
		args         []string
		want         string
	}{
		{"sole peer without default", "[peers.only]\nurl=%q\n", nil, "only cache:"},
		{"sole peer with stale default", "default_peer='gone'\n[peers.only]\nurl=%q\n", nil, "only cache:"},
		{"explicit peer", "default_peer='wrong'\n[peers.wrong]\nurl='http://wrong.invalid'\n[peers.only]\nurl=%q\n", []string{"--on", "only"}, "only cache:"},
		{"explicit URL", "default_peer='wrong'\n[peers.wrong]\nurl='http://wrong.invalid'\n[peers.only]\nurl=%q\n", []string{"--url"}, " cache:"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, dryRun := range []bool{false, true} {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					var req proto.CacheGCRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					if req.DryRun != dryRun {
						t.Errorf("dry run = %v, want %v", req.DryRun, dryRun)
					}
					json.NewEncoder(w).Encode(proto.CacheGCResult{Policies: &proto.CacheGCPolicies{}, DryRun: req.DryRun})
				}))
				defer server.Close()
				writeClientConfig(t, fmt.Sprintf(test.config, server.URL))
				args := append([]string{"cache"}, test.args...)
				if test.name == "explicit URL" {
					args = append(args, server.URL)
				}
				if dryRun {
					args = append(args, "--dry-run")
				}
				var stdout, stderr bytes.Buffer
				if code := cmdGCTo(args, &stdout, &stderr); code != 0 || calls != 1 || !strings.Contains(stdout.String(), test.want) {
					t.Fatalf("gc %v = %d, calls=%d stdout=%q stderr=%q", args, code, calls, stdout.String(), stderr.String())
				}
			}
		})
	}
}

func TestGCOverviewExplainsPoliciesAndScope(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}} {
		var stdout, stderr bytes.Buffer
		cmdGCTo(args, &stdout, &stderr)
		for _, want := range []string{"snapshot blobs and named caches", "--older-than DURATION or --keep N", "local", "all categories, not all runners", "--on PEER", "--dry-run"} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("gc %v help missing %q: %s", args, want, stderr.String())
			}
		}
	}
}

func TestGCCachePreviewReportsRunnerPolicies(t *testing.T) {
	for _, test := range []struct {
		name, response string
		want           []string
	}{
		{"separate policies", `{"dry_run":true,"policies":{"snapshot":{"max_bytes":1073741824,"ttl_seconds":604800},"named":{"max_bytes":2147483648,"ttl_seconds":1209600}}}`, []string{"snapshot cache policy: expire after 7d unused; budget 1.0 GiB", "named cache policy: expire after 14d unused; budget 2.0 GiB", "leased named caches are protected"}},
		{"snapshot disabled", `{"dry_run":true,"policies":{"named":{"max_bytes":2147483648,"ttl_seconds":1209600}}}`, []string{"snapshot cache policy: collection disabled", "named cache policy: expire after 14d"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, test.response) }))
			defer server.Close()
			var stdout, stderr bytes.Buffer
			if code := cmdGCTo([]string{"cache", "--url", server.URL, "--dry-run"}, &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			for _, want := range test.want {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("missing %q in %s", want, stdout.String())
				}
			}
			if !strings.Contains(stdout.String(), server.URL+" cache: would remove") {
				t.Errorf("missing runner and preview: %s", stdout.String())
			}
		})
	}
}
