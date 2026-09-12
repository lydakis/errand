package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/daemon"
	"github.com/lydakis/errand/internal/proto"
)

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
	if code := cmdPushTo(append(args, "--workspace", "missing"), &out, &stderr); code == 0 || !strings.Contains(stderr.String(), "404") {
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
