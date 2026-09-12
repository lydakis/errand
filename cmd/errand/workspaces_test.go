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
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/daemon"
	"github.com/lydakis/errand/internal/proto"
)

func TestWorkspaceCommandsCreateReuseAndRemove(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	server := httptest.NewServer(d.Handler())
	defer server.Close()
	writeClientConfig(t, fmt.Sprintf("default_peer = 'test'\n[peers.test]\nurl = %q\n[profiles.dev.run]\npeer = 'test'\nworkspace = 'experiment'\n[profiles.other.run]\nworkspace = 'missing'\n", server.URL))
	root := t.TempDir()
	t.Chdir(root)
	for name, value := range map[string]string{".errandignore": "", ".errand.toml": "[caches]\ncompiler = 'target'\n", "file": "initial", "target/local": "do not upload"} {
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out, stderr bytes.Buffer
	if code := cmdConfigTo([]string{"--profile", "dev", "--json"}, &out, &stderr); code != 0 {
		t.Fatalf("inspect profile: %d %s", code, &stderr)
	}
	var effective config.EffectiveRun
	if err := json.Unmarshal(out.Bytes(), &effective); err != nil || effective.Workspace != "experiment" || !strings.Contains(effective.Sources["workspace"], "profiles.dev") {
		t.Fatalf("workspace inspection: %+v %v", effective, err)
	}
	out.Reset()
	// A selected profile must never fall back to an ephemeral job.
	if code := cmdRun([]string{"--profile", "dev", "--", "true"}); code == 0 {
		t.Fatal("missing profile workspace silently created a job")
	}
	if code := cmdWorkspacesTo([]string{"create", "--profile", "dev", "--json", "experiment"}, &out, &stderr); code != 0 {
		t.Fatalf("create: %d %s", code, &stderr)
	}
	var workspace proto.Workspace
	if err := json.Unmarshal(out.Bytes(), &workspace); err != nil || len(workspace.Selection.Caches) != 1 {
		t.Fatalf("creation did not use configuration: %+v %v", workspace, err)
	}
	if err := os.WriteFile("file", []byte("local"), 0600); err != nil {
		t.Fatal(err)
	}
	for i, script := range []string{"test \"$(cat file)\" = initial && test ! -e target/local && echo reused > target/remote && echo changed > file", "test \"$(cat file)\" = changed && test \"$(cat target/remote)\" = reused"} {
		args := []string{"--profile", "dev"}
		if i == 1 {
			args = []string{"--profile", "other", "--workspace", "experiment"}
		}
		if code := cmdRun(append(args, "--no-apply", "--", "/bin/sh", "-c", script)); code != 0 {
			t.Fatalf("persistent run: %d", code)
		}
	}
	// Normal invocations still start from a fresh snapshot of current local files.
	if code := cmdRun([]string{"--no-apply", "--", "/bin/sh", "-c", "test \"$(cat file)\" = local; echo ephemeral > file"}); code != 0 {
		t.Fatalf("ephemeral run: %d", code)
	}
	out.Reset()
	stderr.Reset()
	if code := cmdWorkspacesTo([]string{"--json"}, &out, &stderr); code != 0 {
		t.Fatalf("list: %d %s", code, &stderr)
	}
	var rows []proto.WorkspaceSummary
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 1 || len(rows[0].JobIDs) != 0 {
		t.Fatalf("list: %s %v", out.String(), err)
	}
	out.Reset()
	if code := cmdDfTo([]string{"--on", "test", "--json"}, &out, &stderr); code != 0 {
		t.Fatalf("df: %d %s", code, &stderr)
	}
	var inventory []dfRow
	if err := json.Unmarshal(out.Bytes(), &inventory); err != nil || len(inventory) < 1 || inventory[0].Workspaces == nil || inventory[0].Workspaces.Items != 1 {
		t.Fatalf("df: %s %v", out.String(), err)
	}
	out.Reset()
	if code := cmdDfTo([]string{"--on", "test", "--verbose", "--json"}, &out, &stderr); code != 0 {
		t.Fatalf("detailed df: %d %s", code, &stderr)
	}
	if err := json.Unmarshal(out.Bytes(), &inventory); err != nil || inventory[0].Details == nil || len(inventory[0].Details.Workspaces) != 1 {
		t.Fatalf("workspace detail missing: %s %v", &out, err)
	}
	detail := inventory[0].Details.Workspaces[0]
	if detail.ID != workspace.ID || detail.WorkingBytes == 0 || detail.BaseBytes == 0 || detail.MetadataBytes == 0 || detail.Bytes != detail.WorkingBytes+detail.BaseBytes+detail.MetadataBytes || detail.Bytes != inventory[0].Workspaces.Bytes {
		t.Fatalf("workspace accounting: %+v", detail)
	}
	if len(inventory[0].Details.NamedCaches) != 1 || len(inventory[0].Details.Jobs) != 3 {
		t.Fatalf("missing other storage details: %+v", inventory[0].Details)
	}
	out.Reset()
	if code := cmdDfTo([]string{"--on", "test", "-v"}, &out, &stderr); code != 0 || !strings.Contains(out.String(), "experiment") || !strings.Contains(out.String(), "compiler") {
		t.Fatalf("verbose table: %d %s %s", code, &out, &stderr)
	}
	for _, flags := range [][]string{{"-a"}, {"--last", "1"}, {}} {
		out.Reset()
		args := append([]string{"--on", "test", "--workspace", "experiment", "--json"}, flags...)
		if code := cmdPsTo(args, &out, &stderr); code != 0 {
			t.Fatalf("filtered ps: %d %s", code, &stderr)
		}
		var jobs []psRow
		if err := json.Unmarshal(out.Bytes(), &jobs); err != nil {
			t.Fatal(err)
		}
		want := 0
		if len(flags) > 0 {
			want = 2
			if flags[0] == "--last" {
				want = 1
			}
		}
		if len(jobs) != want {
			t.Fatalf("filtered history %v: %s", flags, &out)
		}
		for _, job := range jobs {
			if job.WorkspaceID != workspace.ID {
				t.Fatalf("foreign job in filtered history: %+v", job)
			}
		}
	}
	if code := cmdWorkspacesTo([]string{"rm", "experiment"}, &out, &stderr); code != 0 {
		t.Fatalf("rm: %d %s", code, &stderr)
	}
	if _, err := client.GetWorkspace(server.URL, "experiment"); err == nil {
		t.Fatal("workspace still exists")
	}
	if code := cmdWorkspacesTo([]string{"create", "experiment"}, &out, &stderr); code != 0 {
		t.Fatalf("recreate: %d %s", code, &stderr)
	}
	for _, selector := range []string{"experiment", workspace.ID} {
		out.Reset()
		if code := cmdPsTo([]string{"--on", "test", "--workspace", selector, "-a", "--json"}, &out, &stderr); code != 0 {
			t.Fatalf("history after recreation: %d %s", code, &stderr)
		}
		var jobs []psRow
		if err := json.Unmarshal(out.Bytes(), &jobs); err != nil {
			t.Fatal(err)
		}
		want := 0
		if selector == workspace.ID {
			want = 2
		}
		if len(jobs) != want {
			t.Fatalf("recreated name mixed histories: %s", &out)
		}
	}
}

func TestWorkspaceCommandValidation(t *testing.T) {
	for _, name := range []string{"AAAAAAAAAAAAAAAAAAAAAAAAAA", "01M1ZJ9RDFZTK6EKZJ5PFDD3K8"} {
		var out bytes.Buffer
		if code := cmdWorkspacesTo([]string{"create", name}, &out, &out); code != 2 {
			t.Fatalf("accepted an ID-shaped name %q: %d %s", name, code, &out)
		}
	}
	for _, args := range [][]string{{"create", "../outside"}, {"create", "--no-snapshot", "--include-all", "experiment"}, {"create"}, {"rm"}, {"list", "extra"}, {"unknown"}} {
		var out, stderr bytes.Buffer
		if code := cmdWorkspacesTo(args, &out, &stderr); code != 2 {
			t.Fatalf("accepted %v: %d %s", args, code, &stderr)
		}
	}
	for _, args := range [][]string{{"--workspace", ""}, {"--workspace", "missing", "--no-snapshot"}, {"--workspace", "missing", "--include-all"}} {
		if code := cmdRun(append(args, "--", "true")); code != 2 {
			t.Fatalf("accepted %v: %d", args, code)
		}
	}
}

func TestProfileWorkspaceRejectsIncompatibleRunOptions(t *testing.T) {
	writeClientConfig(t, "default_peer = 'test'\n[peers.test]\nurl = 'http://runner.invalid'\n[profiles.dev.run]\nworkspace = 'development'\n[profiles.ephemeral.run]\npeer = 'test'\n")
	t.Chdir(t.TempDir())
	for _, flags := range [][]string{{"--no-snapshot"}, {"--include-all"}, {"--where", "*"}, {"--workspace", ""}} {
		args := append([]string{"--profile", "dev"}, flags...)
		if code := cmdRun(append(args, "--", "true")); code != 2 {
			t.Fatalf("run %v: %d", flags, code)
		}
	}
	var out, stderr bytes.Buffer
	if code := cmdPushTo([]string{"--profile", "ephemeral"}, &out, &stderr); code != 2 || !strings.Contains(stderr.String(), "run.workspace") {
		t.Fatalf("push without workspace: %d %s", code, &stderr)
	}
	if err := os.WriteFile(".errand.toml", []byte("[run]\nwhere = '*'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := cmdPushTo([]string{"--profile", "dev"}, &out, &stderr); code != 2 || !strings.Contains(stderr.String(), "pinned peer") {
		t.Fatalf("push with automatic placement: %d %s", code, &stderr)
	}
}
