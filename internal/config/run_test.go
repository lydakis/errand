package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runFixture(t *testing.T, personal, project string) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := ClientPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(personal), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errand.toml"), []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

const personalPeers = `default_peer = "linux"
apply_on_success = true
[peers.linux]
url = "http://linux:7443"
[peers.mac]
ssh = "mac-host"
remote_command = "/opt/bin/errand"
remote_socket = "/srv/errand/runner.sock"
`

func TestResolveRunPrecedenceAndProvenance(t *testing.T) {
	root := runFixture(t, personalPeers, "[run]\npeer = 'mac'\n[changes]\napply_on_success = false\n")
	for _, tc := range []struct {
		name         string
		overrides    RunOverrides
		peer, source string
		apply        bool
	}{
		{"workspace", RunOverrides{}, "mac", "workspace", false},
		{"explicit peer", RunOverrides{Peer: "linux"}, "linux", "cli", false},
		{"explicit URL", RunOverrides{URL: "http://other:7443/"}, "http://other:7443", "cli", false},
		{"explicit apply", RunOverrides{ApplyOnSuccess: new(true)}, "mac", "workspace", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveRun(root, tc.overrides)
			if err != nil {
				t.Fatal(err)
			}
			if got.Peer != tc.peer || got.ApplyOnSuccess != tc.apply || !strings.HasPrefix(got.Sources["peer"], tc.source) {
				t.Fatalf("resolved config = %+v", got)
			}
			if tc.overrides.ApplyOnSuccess == nil && !strings.Contains(got.Sources["apply_on_success"], ".errand.toml") {
				t.Fatalf("missing workspace provenance: %+v", got.Sources)
			}
			if got.Peer == "mac" && (got.URL != "ssh://mac-host" || got.RemoteCommand != "/opt/bin/errand" || got.RemoteSocket != "/srv/errand/runner.sock") {
				t.Fatalf("SSH routing was lost: %+v", got)
			}
			if got.Peer != "mac" && (got.RemoteCommand != "" || got.RemoteSocket != "") {
				t.Fatal("inherited another peer's SSH options")
			}
		})
	}
}

func TestResolveRunPersonalAndSafeDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, setting, project, source string
		want                           bool
	}{
		{"safe default", "", "", "default", false},
		{"personal false", "apply_on_success = false\n", "", "personal", false},
		{"personal true", "apply_on_success = true\n", "", "personal", true},
		{"workspace true", "apply_on_success = false\n", "[changes]\napply_on_success = true\n", "workspace", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			personal := "default_peer = 'linux'\n"
			personal += tc.setting
			personal += "[peers.linux]\nurl = 'http://linux:7443'\n"
			root := runFixture(t, personal, tc.project)
			got, err := ResolveRun(root, RunOverrides{})
			if err != nil {
				t.Fatal(err)
			}
			if got.Peer != "linux" || got.ApplyOnSuccess != tc.want {
				t.Fatalf("resolved = %+v", got)
			}
			if !strings.HasPrefix(got.Sources["apply_on_success"], tc.source) {
				t.Fatalf("wrong provenance: %+v", got.Sources)
			}
		})
	}
}

func TestResolveRunSelectedBoundaryAndNoSnapshot(t *testing.T) {
	root := runFixture(t, personalPeers, "[workspace]\nroot = true\n[run]\npeer = 'mac'\n[changes]\napply_on_success = false\n")
	cwd := filepath.Join(root, "child")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".errand.toml"), []byte("[run]\npeer = 'linux'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRun(cwd, RunOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Peer != "mac" || got.Workdir != "child" || got.ApplyOnSuccess {
		t.Fatalf("nested config leaked into selected root: %+v", got)
	}
	got, err = ResolveRun(cwd, RunOverrides{NoSnapshot: true})
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := filepath.EvalSymlinks(cwd)
	if got.Peer != "linux" || got.Root != canonical || got.Workdir != "" || !got.ApplyOnSuccess {
		t.Fatalf("no-snapshot used ancestor settings: %+v", got)
	}
	got, err = ResolveRun(cwd, RunOverrides{Workdir: new("")})
	if err != nil || got.Workdir != "" || got.Sources["workdir"] != "cli: --workdir" {
		t.Fatalf("explicit root workdir lost: %+v, %v", got, err)
	}
}

func TestResolveRunUnknownWorkspacePeerDoesNotFallBack(t *testing.T) {
	for _, peer := range []string{"missing", ""} {
		t.Run(peer, func(t *testing.T) {
			root := runFixture(t, personalPeers, "[run]\npeer = '"+peer+"'\n")
			if _, err := ResolveRun(root, RunOverrides{}); err == nil {
				t.Fatal("invalid preferred peer silently fell back")
			}
			if _, err := ResolveRun(root, RunOverrides{Peer: "linux"}); err != nil {
				t.Fatalf("explicit peer did not override workspace: %v", err)
			}
		})
	}
}

func TestResolveRunIgnoresPreferencesFromUntrustedDiscovery(t *testing.T) {
	root := runFixture(t, personalPeers, "[run]\npeer = 'mac'\n[changes]\napply_on_success = false\n")
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	got, err := ResolveRun(root, RunOverrides{})
	if err != nil || got.Peer != "linux" || !got.ApplyOnSuccess {
		t.Fatalf("untrusted workspace preferences applied: %+v, %v", got, err)
	}
	got, err = ResolveRun(root, RunOverrides{WorkspaceRoot: root})
	if err != nil || got.Peer != "mac" || got.ApplyOnSuccess {
		t.Fatalf("explicit shared root ignored: %+v, %v", got, err)
	}
}

func TestPeerReplacementPreservesExplicitFalseApplyPreference(t *testing.T) {
	runFixture(t, "default_peer = 'test'\napply_on_success = false\n[peers.test]\nurl = 'http://old:7443'\n", "")
	path, err := ClientPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AddPeer(path, "test", Peer{URL: "http://new:7443"}, true); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadClient()
	if err != nil || cfg.ApplyOnSuccess == nil || *cfg.ApplyOnSuccess {
		t.Fatalf("replacement lost explicit false: %+v, %v", cfg, err)
	}
}
