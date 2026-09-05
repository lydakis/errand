package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfilePrecedenceAndExplicitSelection(t *testing.T) {
	root := runFixture(t, personalPeers+"\n[profiles.dev.run]\npeer = 'mac'\nworkdir = 'build'\n[profiles.dev.changes]\napply_on_success = false\n", "[run]\npeer = 'linux'\n[changes]\napply_on_success = true\n")
	for _, tc := range []struct {
		name          string
		cli           RunOverrides
		peer, workdir string
		apply         bool
	}{
		{"inactive", RunOverrides{}, "linux", "", true},
		{"profile", RunOverrides{Profile: "dev"}, "mac", "build", false},
		{"CLI", RunOverrides{Profile: "dev", Peer: "linux", Workdir: new(""), ApplyOnSuccess: new(true)}, "linux", "", true},
		{"URL", RunOverrides{Profile: "dev", URL: "ssh://other"}, "ssh://other", "build", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveRun(root, tc.cli)
			if err != nil {
				t.Fatal(err)
			}
			if got.Peer != tc.peer || got.Workdir != tc.workdir || got.ApplyOnSuccess != tc.apply || got.Profile != tc.cli.Profile {
				t.Fatalf("resolved: %+v", got)
			}
			if tc.name == "profile" {
				for _, key := range []string{"peer", "workdir", "apply_on_success"} {
					if !strings.Contains(got.Sources[key], "profiles.dev") {
						t.Fatalf("missing profile provenance: %+v", got.Sources)
					}
				}
			}
		})
	}
}

func TestWorkspaceProfileReplacesPersonalDefinition(t *testing.T) {
	root := runFixture(t, personalPeers+"\n[profiles.dev.run]\npeer = 'mac'\nworkdir = 'hidden'\n[profiles.dev.changes]\napply_on_success = false\n", "[profiles.dev.run]\npeer = 'linux'\n")
	got, err := ResolveRun(root, RunOverrides{Profile: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Peer != "linux" || got.Workdir != "" || !got.ApplyOnSuccess || !strings.Contains(got.Sources["peer"], ".errand.toml") {
		t.Fatalf("personal profile leaked through replacement: %+v", got)
	}
}

func TestProfilesRespectWorkspaceBoundary(t *testing.T) {
	root := runFixture(t, personalPeers, "[workspace]\nroot = true\n[profiles.dev.run]\npeer = 'mac'\n")
	cwd := filepath.Join(root, "child")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".errand.toml"), []byte("[profiles.dev.run]\npeer = 'linux'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRun(cwd, RunOverrides{Profile: "dev"})
	if err != nil || got.Peer != "mac" {
		t.Fatalf("nested profile overrode selected root: %+v, %v", got, err)
	}
	got, err = ResolveRun(cwd, RunOverrides{Profile: "dev", NoSnapshot: true})
	if err != nil || got.Peer != "linux" {
		t.Fatalf("no-snapshot used ancestor profile: %+v, %v", got, err)
	}
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	if _, err := ResolveRun(root, RunOverrides{Profile: "dev"}); err == nil {
		t.Fatal("untrusted discovered profile was used")
	}
}

func TestProfileErrorsAndWorkdirOverride(t *testing.T) {
	root := runFixture(t, personalPeers+"\n[profiles.dev.run]\nworkdir = 'build'\n", "")
	if _, err := ResolveRun(root, RunOverrides{Profile: "missing", Peer: "linux"}); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("unknown profile: %v", err)
	}
	if _, err := ResolveRun(root, RunOverrides{Profile: "dev", NoSnapshot: true}); err == nil {
		t.Fatal("profile workdir bypassed no-snapshot restriction")
	}
	if _, err := ResolveRun(root, RunOverrides{Profile: "dev", NoSnapshot: true, Workdir: new(".")}); err != nil {
		t.Fatal(err)
	}
}

func TestProfileSchemaAndPersistence(t *testing.T) {
	for _, body := range []string{"peer = 'mac'", "[run]\nurl = 'http://other'", "[run]\nworkdir = false", "[changes]\napply_on_sucess = true"} {
		for _, location := range []string{"personal", "workspace"} {
			t.Run(location+body, func(t *testing.T) {
				profile := "\n[profiles.dev]\n" + strings.ReplaceAll(body, "[", "[profiles.dev.") + "\n"
				personal, project := personalPeers, ""
				if location == "personal" {
					personal += profile
				} else {
					project = profile
				}
				root := runFixture(t, personal, project)
				if _, err := ResolveRun(root, RunOverrides{Profile: "dev"}); err == nil {
					t.Fatal("invalid profile accepted")
				}
			})
		}
	}
	root := runFixture(t, personalPeers+"\n[profiles.empty]\n[profiles.dev.run]\npeer = 'mac'\nworkdir = ''\n[profiles.dev.changes]\napply_on_success = false\n", "")
	path, _ := ClientPath()
	if _, err := AddPeer(path, "linux", Peer{URL: "http://updated:7443"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := RemovePeer(path, "linux"); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRun(root, RunOverrides{Profile: "dev"})
	if err != nil || got.Peer != "mac" || got.ApplyOnSuccess || !strings.Contains(got.Sources["workdir"], "profiles.dev") {
		t.Fatalf("profile lost during peer edits: %+v, %v", got, err)
	}
	if _, err := ResolveRun(root, RunOverrides{Profile: "empty", Peer: "mac"}); err != nil {
		t.Fatalf("empty profile lost during peer edits: %v", err)
	}
}
