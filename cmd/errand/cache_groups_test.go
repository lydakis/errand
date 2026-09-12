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

func TestCacheGroupsReuseAcrossJobsAndFreezeAtCreation(t *testing.T) {
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
	write := func(name, value string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(".errandignore", "")
	write(".errand.toml", `[caches.dependencies]
roots = [".", "packages/*"]
path = "node_modules"
[caches.builds]
roots = ["packages/*"]
path = "dist"
[profiles.dev.run]
workspace = "dev"
`)
	write("packages/core/source", "original")
	inspect := func() config.EffectiveRun {
		t.Helper()
		var out, stderr bytes.Buffer
		if code := cmdConfigTo([]string{"--json"}, &out, &stderr); code != 0 {
			t.Fatalf("config: %d %s", code, &stderr)
		}
		var got config.EffectiveRun
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		for _, b := range got.Caches {
			if !strings.Contains(got.CacheSources[b.Name], "caches.") {
				t.Fatalf("missing declaration: %+v", got)
			}
		}
		out.Reset()
		if code := cmdConfigTo(nil, &out, &stderr); code != 0 || !strings.Contains(out.String(), "packages/core/node_modules") || !strings.Contains(out.String(), "caches.dependencies") {
			t.Fatalf("human config: %d %s %s", code, &out, &stderr)
		}
		return got
	}
	first := inspect()
	if len(first.Caches) != 3 {
		t.Fatal(first.Caches)
	}
	run := func(args []string, script string) {
		t.Helper()
		if code := cmdRun(append(append([]string{"--no-apply"}, args...), "--", "/bin/sh", "-c", script)); code != 0 {
			t.Fatalf("job: %d", code)
		}
	}
	// Simulate installation, then compilation in fresh independent jobs. Both
	// dependency and output paths are absent locally throughout the sequence.
	run(nil, "printf installed > node_modules/state; printf dependency > packages/core/node_modules/state")
	run(nil, "test \"$(cat node_modules/state)\" = installed && test \"$(cat packages/core/node_modules/state)\" = dependency && cp packages/core/source packages/core/dist/output")
	write("packages/new/source", "new")
	second := inspect()
	if len(second.Caches) != 5 {
		t.Fatal(second.Caches)
	}
	for _, old := range first.Caches {
		found := false
		for _, b := range second.Caches {
			if b == old {
				found = true
			}
		}
		if !found {
			t.Fatalf("existing binding changed: %+v", old)
		}
	}
	run(nil, "test \"$(cat packages/core/dist/output)\" = original && test \"$(cat packages/core/node_modules/state)\" = dependency && test ! -e packages/new/node_modules/state && printf new-dependency > packages/new/node_modules/state")
	var out, stderr bytes.Buffer
	if code := cmdWorkspacesTo([]string{"create", "--profile", "dev", "--json", "dev"}, &out, &stderr); code != 0 {
		t.Fatalf("create: %d %s", code, &stderr)
	}
	var created proto.Workspace
	if err := json.Unmarshal(out.Bytes(), &created); err != nil || len(created.Selection.Caches) != 5 {
		t.Fatalf("creation: %+v %v", created, err)
	}
	// A third package is delivered by push without changing frozen bindings.
	write("packages/third/source", "third")
	out.Reset()
	if code := cmdPushTo([]string{"--profile", "dev", "--apply"}, &out, &stderr); code != 0 {
		t.Fatalf("push: %d %s", code, &stderr)
	}
	run([]string{"--profile", "dev"}, "test \"$(cat packages/new/node_modules/state)\" = new-dependency && test \"$(cat packages/third/source)\" = third && test ! -e packages/third/node_modules")
	after, err := client.GetWorkspace(server.URL, "dev")
	if err != nil || len(after.Selection.Caches) != 5 {
		t.Fatalf("bindings changed: %+v %v", after.Selection.Caches, err)
	}
	for _, b := range second.Caches {
		if _, err := os.Stat(filepath.Join(root, b.Path)); !os.IsNotExist(err) {
			t.Fatalf("local cache created: %s %v", b.Path, err)
		}
	}
}
