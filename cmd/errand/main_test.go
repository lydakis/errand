package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func writeClientConfig(t *testing.T, body string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	dir := filepath.Join(root, "errand")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveHandlePreservesRawURL(t *testing.T) {
	id := proto.NewULID()
	peerURL, label, jobID, err := resolveHandle("http://runner:9000/"+id, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if peerURL != "http://runner:9000" || label != peerURL || jobID != id {
		t.Fatalf("resolved raw handle = (%q, %q, %q), want port-preserving URL and %q", peerURL, label, jobID, id)
	}
}

func TestResolvePeerTargetUsesConfiguredDefaultAlias(t *testing.T) {
	writeClientConfig(t, `default_peer = "cabal"
[peers.cabal]
url = "http://cabal:7443"
`)
	peerURL, label, err := resolvePeerTarget("", "")
	if err != nil {
		t.Fatal(err)
	}
	if peerURL != "http://cabal:7443" || label != "cabal" {
		t.Fatalf("default peer target = (%q, %q), want configured alias and URL", peerURL, label)
	}
}

func TestResolvePeerTargetRejectsConflictingSelectors(t *testing.T) {
	if _, _, err := resolvePeerTarget("http://runner:7443", "cabal"); err == nil {
		t.Fatal("--url and --on were accepted together")
	}
}

func TestResolveHandleFailsClosed(t *testing.T) {
	writeClientConfig(t, `default_peer = "cabal"
[peers.cabal]
url = "http://cabal:7443"
`)
	id := proto.NewULID()
	for name, tc := range map[string][3]string{
		"unknown alias":    {"cabla/" + id, "", ""},
		"conflicting on":   {"cabal/" + id, "", "other"},
		"conflicting urls": {"http://atlas:7443/" + id, "http://cabal:7443", ""},
		"invalid job id":   {"cabal/not-a-ulid", "", ""},
		"dual selectors":   {id, "http://runner:7443", "cabal"},
	} {
		t.Run(name, func(t *testing.T) {
			args, rawURL, on := tc[0], tc[1], tc[2]
			if _, _, _, err := resolveHandle(args, rawURL, on); err == nil {
				t.Fatalf("resolveHandle(%q, %q, %q) succeeded", args, rawURL, on)
			}
		})
	}
}

func TestResolveHandleMatchingURLSelector(t *testing.T) {
	id := proto.NewULID()
	peerURL, label, jobID, err := resolveHandle(
		"http://runner:9000/"+id, "http://runner:9000/", "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if peerURL != "http://runner:9000" || label != peerURL || jobID != id {
		t.Fatalf("matching URL selector resolved to (%q, %q, %q)", peerURL, label, jobID)
	}
}

func TestResolveHandleURLOverrideUsesEffectiveURLLabel(t *testing.T) {
	id := proto.NewULID()
	peerURL, label, jobID, err := resolveHandle("cabal/"+id, "http://runner:9000", "")
	if err != nil {
		t.Fatal(err)
	}
	if peerURL != "http://runner:9000" || label != peerURL || jobID != id {
		t.Fatalf("URL override resolved to (%q, %q, %q)", peerURL, label, jobID)
	}
}

func TestCmdPsReportsPartialConfiguredPeerFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `[]`)
	}))
	defer server.Close()
	writeClientConfig(t, fmt.Sprintf(`default_peer = "good"
[peers.good]
url = %q
[peers.broken]
url = ""
`, server.URL))
	var stdout, stderr bytes.Buffer
	if code := cmdPsTo(nil, &stdout, &stderr); code == 0 {
		t.Fatalf("partial ps returned success; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "PEER") || !strings.Contains(stderr.String(), "broken") {
		t.Fatalf("partial ps diagnostics missing; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestCmdRunRejectsConflictingSnapshotFlags(t *testing.T) {
	if code := cmdRun([]string{"--no-snapshot", "--include-all", "--", "/bin/true"}); code != 2 {
		t.Fatalf("conflicting snapshot flags exit = %d, want 2", code)
	}
}

func TestCmdRunRejectsWorkdirWithoutSnapshot(t *testing.T) {
	if code := cmdRun([]string{"--no-snapshot", "--workdir", "build", "--", "/bin/true"}); code != 2 {
		t.Fatalf("no-snapshot workdir exit = %d, want 2", code)
	}
}
