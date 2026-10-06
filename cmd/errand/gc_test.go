package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
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

func TestGCChangesSkipsDeletedWorkspaceWithoutFailing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/snapshot/diff") {
			var request proto.SnapshotDiffRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				http.Error(w, "invalid snapshot negotiation", http.StatusBadRequest)
				return
			}
			missing := make([]string, 0, len(request.Blobs))
			for _, blob := range request.Blobs {
				missing = append(missing, blob.SHA256)
			}
			json.NewEncoder(w).Encode(proto.SnapshotDiffResponse{Missing: missing})
			return
		}
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(proto.Workspace{ID: strings.TrimPrefix(r.URL.Path, "/v0/workspaces/")})
	}))
	defer server.Close()
	if _, err := client.CreateWorkspace(client.RunOptions{PeerURL: server.URL, Root: root}, "gone"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"changes", "--older-than", "1d", "--dry-run"},
		{"changes", "--older-than", "1d"},
	} {
		var stdout, stderr bytes.Buffer
		if code := cmdGCTo(args, &stdout, &stderr); code != 0 ||
			!strings.Contains(stdout.String(), " 0 records") || !strings.HasSuffix(stdout.String(), "(0 protected, 0 failed)\n") ||
			stderr.String() != "errand: local change gc: skipped 1 workspace transfer record whose workspace was moved or deleted\n" {
			t.Fatalf("gc %v = %d, stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}

// Job records from an earlier errand (here the declared-output format that
// change capture replaced) cannot be decoded and protect nothing, so gc
// changes collects them by age like any other record.
func TestGCChangesCollectsJobRecordsFromEarlierErrand(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	jobs := filepath.Join(state, "errand", "jobs")
	downloads := filepath.Join(state, "errand", "downloads")
	for _, dir := range []string{jobs, downloads} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	peer := "http://runner.test"
	sum := sha256.Sum256([]byte(peer))
	old := time.Now().Add(-48 * time.Hour)
	writeRecord := func(modified time.Time) string {
		id := proto.NewULID()
		key := hex.EncodeToString(sum[:16]) + "-" + id
		path := filepath.Join(jobs, key+".json")
		record := fmt.Sprintf(`{"version":1,"job_id":%q,"peer_url":%q,"root":"/src/app","root_identity":{"device":1,"inode":2},"outputs":[{"path":"dist"}],"baselines":[]}`, id, peer)
		if err := os.WriteFile(path, []byte(record), 0o600); err != nil {
			t.Fatal(err)
		}
		download := filepath.Join(downloads, key)
		if err := os.Mkdir(download, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(download, "bundle.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{path, download} {
			if err := os.Chtimes(p, modified, modified); err != nil {
				t.Fatal(err)
			}
		}
		return path
	}
	var legacy []string
	for range 4 {
		legacy = append(legacy, writeRecord(old))
	}
	recent := writeRecord(time.Now())

	gc := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := cmdGCTo(append([]string{"changes", "--older-than", "1d"}, args...), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	if code, stdout, stderr := gc("--dry-run"); code != 0 || stderr != "" ||
		!strings.HasPrefix(stdout, "local changes: would remove 4 records") || !strings.HasSuffix(stdout, "(0 protected, 0 failed)\n") {
		t.Fatalf("dry run = %d, stdout=%q stderr=%q", code, stdout, stderr)
	}
	if code, stdout, stderr := gc(); code != 0 || stderr != "" ||
		!strings.HasPrefix(stdout, "local changes: removed 4 records") || !strings.HasSuffix(stdout, "(0 protected, 0 failed)\n") {
		t.Fatalf("gc = %d, stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, path := range legacy {
		key := strings.TrimSuffix(filepath.Base(path), ".json")
		for _, p := range []string{path, filepath.Join(downloads, key)} {
			if _, err := os.Lstat(p); !os.IsNotExist(err) {
				t.Fatalf("%s survived gc: %v", p, err)
			}
		}
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("record newer than --older-than was collected: %v", err)
	}

	// A record gc cannot collect is named with the reason, not just counted.
	stuck := writeRecord(old)
	locks := filepath.Join(state, "errand", "locks")
	if err := os.RemoveAll(locks); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(locks, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--dry-run"}, nil} {
		code, stdout, stderr := gc(args...)
		if code != 1 || !strings.HasPrefix(stderr, "errand: local change gc: "+stuck+": ") ||
			!strings.HasSuffix(stdout, "(0 protected, 1 failed)\n") {
			t.Fatalf("gc %v with unusable locks = %d, stdout=%q stderr=%q", args, code, stdout, stderr)
		}
	}
	if _, err := os.Stat(stuck); err != nil {
		t.Fatalf("record that failed collection was removed: %v", err)
	}
}
