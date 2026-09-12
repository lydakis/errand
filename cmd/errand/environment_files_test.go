package main

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/daemon"
	"github.com/lydakis/errand/internal/proto"
)

func TestWorkspaceSourceCommandsDoNotLoadEnvironmentFiles(t *testing.T) {
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
	for name, body := range map[string]string{
		".errandignore": "*.env\n",
		".errand.toml":  "[profiles.dev.run]\nworkspace='push-test'\n[profiles.dev.env]\nfiles=['unavailable.env']\n",
		"value":         "initial",
	} {
		if err := os.WriteFile(name, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.CreateWorkspace(client.RunOptions{PeerURL: server.URL, Root: root}, "push-test"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"missing", "malformed"} {
		t.Run(state, func(t *testing.T) {
			if state == "malformed" {
				if err := os.WriteFile("unavailable.env", []byte("KEY='unclosed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var out, stderr bytes.Buffer
			if code := cmdWorkspacesTo([]string{"create", "--profile", "dev", "create-" + state}, &out, &stderr); code != 0 {
				t.Errorf("create: %d %s", code, &stderr)
			}
			if err := os.WriteFile("value", []byte(state), 0600); err != nil {
				t.Fatal(err)
			}
			stderr.Reset()
			if code := cmdPushTo([]string{"--profile", "dev", "--apply"}, &out, &stderr); code != 0 {
				t.Fatalf("push: %d %s", code, &stderr)
			}
			out.Reset()
			if code := client.Run(client.RunOptions{PeerURL: server.URL, Root: root, Workspace: "push-test", Argv: []string{"cat", "value"}, Stdout: &out, Stderr: &stderr}); code != 0 || out.String() != state {
				t.Fatalf("source was not delivered: %d %s", code, &stderr)
			}
		})
	}
}

func TestEnvironmentFileCLIOverridesAndFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("contacted runner with an invalid environment file")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	writeClientConfig(t, fmt.Sprintf("default_peer='test'\n[peers.test]\nurl=%q\n[env]\nfiles=['missing.env']\n", server.URL))
	t.Chdir(t.TempDir())
	if err := os.WriteFile("input.env", []byte("FROM_CLI=value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--env-file", "input.env"}, {"--no-env-files"}} {
		var out, stderr bytes.Buffer
		if code := cmdConfigTo(args, &out, &stderr); code != 0 {
			t.Fatalf("override: %d %s", code, &stderr)
		}
	}
	for _, args := range [][]string{{"--env-file", ""}, {"--env-file", "input.env", "--no-env-files"}} {
		var out, stderr bytes.Buffer
		if code := cmdConfigTo(args, &out, &stderr); code == 0 {
			t.Fatalf("accepted invalid options: %v", args)
		}
	}
	if err := os.WriteFile("malformed.env", []byte("KEY='unclosed"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"missing.env", "malformed.env", "."} {
		var out, stderr bytes.Buffer
		if code := cmdConfigTo([]string{"--env-file", path}, &out, &stderr); code == 0 {
			t.Fatalf("config accepted invalid file: %s", path)
		}
		if code := cmdDoctorTo([]string{"--env-file", path, "--json"}, &out, &stderr, func(context.Context, string) (proto.Info, error) {
			t.Fatal("probed with an invalid file")
			return proto.Info{}, nil
		}); code != 1 {
			t.Fatalf("doctor: %d", code)
		}
		if code := cmdRun([]string{"--env-file", path, "--", "true"}); code != 2 {
			t.Fatalf("run accepted invalid file: %s (exit %d)", path, code)
		}
	}
}

func TestEnvironmentFilesReachPersistentAndEphemeralJobs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	state := t.TempDir()
	d, err := daemon.New(daemon.Config{StateDir: state, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	server := httptest.NewServer(d.Handler())
	t.Cleanup(server.Close)
	writeClientConfig(t, fmt.Sprintf("default_peer='test'\n[peers.test]\nurl=%q\n", server.URL))
	root := t.TempDir()
	t.Chdir(root)
	for name, body := range map[string]string{
		".errandignore": "*.env\n.env.local\n",
		".errand.toml":  "[profiles.dev.run]\nworkspace='env-test'\n[profiles.dev.env]\nfiles=['.env.local']\n",
		".env.local":    "FROM_FILE=dummy-file-secret\nORIGINAL=yes\n",
		"override.env":  "FROM_FILE=dummy-override-secret\nCLI_ONLY=yes\n",
		"last.env":      "ORDER=last\n",
	} {
		if err := os.WriteFile(name, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out, stderr bytes.Buffer
	if code := cmdWorkspacesTo([]string{"create", "--profile", "dev", "env-test"}, &out, &stderr); code != 0 {
		t.Fatalf("create: %d %s", code, &stderr)
	}
	if code := cmdRun([]string{"--profile", "dev", "--", "/bin/sh", "-c", `test -n "$FROM_FILE" && test "$ORIGINAL" = yes && test ! -e .env.local`}); code != 0 {
		t.Fatalf("profile file: %d", code)
	}
	// Values are loaded afresh locally, even though persistent runs never push source.
	if err := os.WriteFile(".env.local", []byte("FROM_FILE=dummy-updated-secret\nUPDATED=yes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := cmdRun([]string{"--profile", "dev", "--", "/bin/sh", "-c", `test -n "$FROM_FILE" && test "$UPDATED" = yes && test -z "${ORIGINAL+x}"`}); code != 0 {
		t.Fatalf("updated file: %d", code)
	}
	// CLI paths use the invoking directory, and repeated flags form one replacement list.
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(child)
	if code := cmdRun([]string{"--env-file", "../override.env", "--env-file", "../last.env", "--env", "CLI_ONLY=explicit", "--no-snapshot", "--", "/bin/sh", "-c", `test -n "$FROM_FILE" && test "$CLI_ONLY" = explicit && test "$ORDER" = last`}); code != 0 {
		t.Fatalf("CLI files: %d", code)
	}
	t.Chdir(root)
	if code := cmdRun([]string{"--profile", "dev", "--env-file", "override.env", "--", "/bin/sh", "-c", `test "$CLI_ONLY" = yes && test -z "${UPDATED+x}"`}); code != 0 {
		t.Fatalf("replacement: %d", code)
	}
	if err := filepath.WalkDir(state, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, secret := range []string{"dummy-file-secret", "dummy-override-secret", "dummy-updated-secret"} {
			if strings.Contains(string(raw), secret) {
				t.Fatal("file value persisted in runner metadata")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
