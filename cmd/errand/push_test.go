package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/daemon"
	"github.com/lydakis/errand/internal/proto"
)

func TestPushRetryFlagsPreserveValuesAndPaths(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args, want []string
	}{
		{"bare flags", []string{"--apply", "-conflicts", "--workspace", "dev"}, []string{"--workspace", "dev"}},
		{"explicit values", []string{"--apply=false", "-apply=true", "--conflicts=false", "-conflicts=true", "--workspace", "dev"}, []string{"--workspace", "dev"}},
		{"string values resembling flags", []string{"--workspace", "--apply", "--profile", "--conflicts", "--apply=false"}, []string{"--workspace", "--apply", "--profile", "--conflicts"}},
		{"equals string value", []string{"--workspace=--apply", "--apply=false"}, []string{"--workspace=--apply"}},
		{"unrelated boolean", []string{"--watch", "--json=false", "--apply=false"}, []string{"--watch", "--json=false"}},
		{"explicit separator", []string{"--workspace", "dev", "--apply=false", "--", "--apply"}, []string{"--workspace", "dev", "--", "--apply"}},
		{"positional", []string{"--workspace", "dev", "--apply=false", "path", "--conflicts=false"}, []string{"--workspace", "dev", "path", "--conflicts=false"}},
		{"single dash positional", []string{"--apply=false", "-", "--apply"}, []string{"-", "--apply"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("push", flag.ContinueOnError)
			apply, conflicts := fs.Bool("apply", false, ""), fs.Bool("conflicts", false, "")
			workspace, profile := fs.String("workspace", "", ""), fs.String("profile", "", "")
			fs.Bool("watch", false, "")
			fs.Bool("json", false, "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			originalWorkspace, originalProfile := *workspace, *profile
			originalPaths := slices.Clone(fs.Args())
			got := withoutFlag(fs, tc.args, "apply", "conflicts")
			if !slices.Equal(got, tc.want) {
				t.Fatalf("retry args = %q, want %q", got, tc.want)
			}
			if err := fs.Parse(append([]string{"--apply", "--conflicts"}, got...)); err != nil {
				t.Fatal(err)
			}
			if !*apply || !*conflicts || *workspace != originalWorkspace || *profile != originalProfile || !slices.Equal(fs.Args(), originalPaths) {
				t.Fatalf("retry changed flags or paths: apply=%v conflicts=%v workspace=%q profile=%q paths=%q", *apply, *conflicts, *workspace, *profile, fs.Args())
			}
		})
	}
}

func TestPushUsesConfiguredPeerAndExplicitApply(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	server := httptest.NewServer(d.Handler())
	defer server.Close()
	writeClientConfig(t, fmt.Sprintf("[peers.test]\nurl = %q\n[profiles.dev.run]\npeer = 'test'\nworkspace = 'experiment'\n[profiles.dev.changes]\napply_on_success = true\n", server.URL))
	root := t.TempDir()
	t.Chdir(root)
	for name, body := range map[string]string{".errandignore": "", "value": "initial\n"} {
		if err := os.WriteFile(name, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: server.URL, Root: root}, "experiment")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("value", []byte("local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	args := []string{"--profile", "dev", "--json"}
	if code := cmdPushTo(args, &out, &stderr); code != 0 {
		t.Fatalf("push: %d %s", code, &stderr)
	}
	var result proto.PushResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.WorkspaceID != ws.ID {
		t.Fatalf("result: %+v %v", result, err)
	}
	var run bytes.Buffer
	if code := client.Run(client.RunOptions{PeerURL: server.URL, Root: root, Workspace: ws.Name, Argv: []string{"cat", "value"}, Stdout: &run, Stderr: &stderr}); code != 0 || run.String() != "initial\n" {
		t.Fatalf("profile silently applied push: %d %s %s", code, &run, &stderr)
	}
	out.Reset()
	stderr.Reset()
	if code := cmdPushTo(append(args, "--apply"), &out, &stderr); code != 0 {
		t.Fatalf("apply: %d %s", code, &stderr)
	}
	run.Reset()
	if code := client.Run(client.RunOptions{PeerURL: server.URL, Root: root, Workspace: ws.Name, Argv: []string{"cat", "value"}, Stdout: &run, Stderr: &stderr}); code != 0 || run.String() != "local\n" {
		t.Fatalf("remote: %d %s %s", code, &run, &stderr)
	}
	stderr.Reset()
	if code := cmdPushTo(append(args, "--workspace", "missing"), &out, &stderr); code == 0 || !strings.Contains(stderr.String(), "has no workspace named missing") {
		t.Fatalf("explicit workspace did not override profile: %d %s", code, &stderr)
	}
	// A different checkout cannot accidentally replace this workspace's source.
	t.Chdir(t.TempDir())
	out.Reset()
	stderr.Reset()
	if code := cmdPushTo([]string{"--workspace", ws.Name, "--profile", "dev", "--workspace-root", filepath.Dir(root)}, &out, &stderr); code == 0 {
		t.Fatal("accepted another origin")
	}
}
func TestPushRejectsAmbiguousInterface(t *testing.T) {
	for _, args := range [][]string{{}, {"cabal/JOB"}, {"--workspace", "x", "--conflicts"}, {"--workspace", "x", "a", "b"}} {
		var out, stderr bytes.Buffer
		if code := cmdPushTo(args, &out, &stderr); code != 2 {
			t.Fatalf("%v: %d %s", args, code, &stderr)
		}
	}
}

func TestPushPrintsRunnableRecoveryForEarlierTransferState(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	server := httptest.NewServer(d.Handler())
	defer server.Close()
	writeClientConfig(t, fmt.Sprintf("[peers.test]\nurl = %q\n[profiles.dev.run]\npeer = 'test'\nworkspace = 'api'\n", server.URL))
	root := t.TempDir()
	t.Chdir(root)
	for name, body := range map[string]string{".errandignore": "", "value": "initial\n"} {
		if err := os.WriteFile(name, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out, stderr bytes.Buffer
	if code := cmdWorkspacesTo([]string{"create", "--on", "test", "api"}, &out, &stderr); code != 0 {
		t.Fatalf("create: %d %s", code, &stderr)
	}
	ws, err := client.GetWorkspace(server.URL, "api")
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := filepath.Glob(filepath.Join(state, "errand", "workspace-transfers", "*-"+ws.ID))
	if err != nil || len(dirs) != 1 {
		t.Fatalf("transfer state: %v %v", dirs, err)
	}
	dir := dirs[0]
	// Rewrite the relationship as an earlier errand recorded it: the creation
	// manifest embedded in origin.json, no initial_root and no initial.json.
	var origin map[string]json.RawMessage
	raw, err := os.ReadFile(filepath.Join(dir, "origin.json"))
	if err == nil {
		err = json.Unmarshal(raw, &origin)
	}
	if err != nil {
		t.Fatal(err)
	}
	if origin["initial"], err = os.ReadFile(filepath.Join(dir, "initial.json")); err != nil {
		t.Fatal(err)
	}
	delete(origin, "initial_root")
	if raw, err = json.Marshal(origin); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "origin.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "initial.json")); err != nil {
		t.Fatal(err)
	}
	recovery := fmt.Sprintf("errand workspaces rm --on test %s && errand workspaces create --on test%%s api && rm -r '%s'", ws.ID, dir)
	for _, c := range []struct {
		args    []string
		profile string
	}{
		{[]string{"--on", "test", "--workspace", "api"}, ""},
		{[]string{"--on", "test", "--workspace", "api", "--watch"}, ""},
		{[]string{"--profile", "dev"}, " --profile 'dev'"},
	} {
		stderr.Reset()
		if code := cmdPushTo(c.args, &out, &stderr); code == 0 || !strings.Contains(stderr.String(), "workspace api was created by an earlier errand") ||
			!strings.Contains(stderr.String(), fmt.Sprintf(recovery, c.profile)) {
			t.Fatalf("push %v: %d %s", c.args, code, &stderr)
		}
	}
	stderr.Reset()
	if code := cmdGCTo([]string{"changes", "--older-than", "1d"}, &out, &stderr); code == 0 || !strings.Contains(stderr.String(), "created by an earlier errand") {
		t.Fatalf("gc with earlier state: %d %s", code, &stderr)
	}
	// Run the printed recovery, then push the checkout's current contents.
	if code := cmdWorkspacesTo([]string{"rm", "--on", "test", ws.ID}, &out, &stderr); code != 0 {
		t.Fatalf("rm: %d %s", code, &stderr)
	}
	if code := cmdWorkspacesTo([]string{"create", "--on", "test", "--profile", "dev", "api"}, &out, &stderr); code != 0 {
		t.Fatalf("recreate: %d %s", code, &stderr)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("value", []byte("local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := cmdPushTo([]string{"--profile", "dev", "--apply"}, &out, &stderr); code != 0 {
		t.Fatalf("push after recovery: %d %s", code, &stderr)
	}
	var run bytes.Buffer
	if code := client.Run(client.RunOptions{PeerURL: server.URL, Root: root, Workspace: "api", Argv: []string{"cat", "value"}, Stdout: &run, Stderr: &stderr}); code != 0 || run.String() != "local\n" {
		t.Fatalf("recreated workspace: %d %s %s", code, &run, &stderr)
	}
	out.Reset()
	stderr.Reset()
	if code := cmdGCTo([]string{"changes", "--older-than", "1d"}, &out, &stderr); code != 0 || !strings.Contains(out.String(), "nothing to collect") {
		t.Fatalf("gc after recovery: %d %q %q", code, &out, &stderr)
	}
}
