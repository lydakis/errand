package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
)

func TestLocalRunnerEndToEnd(t *testing.T) {
	bin := buildErrand(t)
	isolateDoctorHost(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir, err := os.MkdirTemp("/tmp", "errand-local-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "runner.sock")
	cfgPath, _ := config.DaemonPath()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("transport = 'local'\nlisten = 'tailnet:7443'\nsocket = %q\nstate_dir = %q\ntailscale_cli = '/not-installed'\n", socket, filepath.Join(dir, "state"))
	if err := os.WriteFile(cfgPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	// A stale service override must not undo local-only mode.
	out, err := exec.Command(bin, "serve", "--config", cfgPath, "--listen", "127.0.0.1:0").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "local-only transport cannot enable") {
		t.Fatalf("network override: %v %s", err, out)
	}
	server := exec.Command(bin, "serve", "--config", cfgPath)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Process.Kill(); server.Wait() })
	target, _ := config.LocalURL(socket)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		info, err := client.ProbeInfo(ctx, target, time.Second)
		if err == nil {
			if !info.SSHDisabled || !info.LocalOnly {
				t.Fatal("local runner advertised SSH access")
			}
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("local runner never became ready: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, err = exec.Command(bin, "_stdio", "--socket", socket).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "SSH transport is disabled") {
		t.Fatalf("SSH bridge accepted local runner: %v %s", err, out)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = root
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("errand %v: %v\n%s\n%s", args, err, &stdout, &stderr)
		}
		return stdout.String(), stderr.String()
	}
	handleFrom := func(logs string) string {
		t.Helper()
		for _, line := range strings.Split(logs, "\n") {
			if strings.HasPrefix(line, "errand: job ") {
				return strings.Fields(line)[2]
			}
		}
		t.Fatalf("missing job handle: %s", logs)
		return ""
	}
	run("config", "--on", "local", "--json")
	run("peers", "--on", "local", "--json")
	run("peers", "--json")
	// Separate CLI processes must retain the same submission/apply identity.
	_, logs := run("--on", "local", "--include-all", "--no-apply", "--", "/bin/sh", "-c", "printf changed > file.txt; printf local-output")
	handle := handleFrom(logs)
	if !strings.HasPrefix(handle, "local/") {
		t.Fatal(handle)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "file.txt")); string(got) != "original" {
		t.Fatal("job changed originating checkout")
	}
	list, _ := run("ps", "--last", "5", "--json")
	if !strings.Contains(list, strings.TrimPrefix(handle, "local/")) {
		t.Fatalf("local job absent from ps: %s", list)
	}
	run("attach", handle)
	export := filepath.Join(t.TempDir(), "export")
	run("fetch", "--output", export, handle)
	if got, _ := os.ReadFile(filepath.Join(export, "file.txt")); string(got) != "changed" {
		t.Fatal("export failed")
	}
	run("fetch", "--apply", handle)
	if got, _ := os.ReadFile(filepath.Join(root, "file.txt")); string(got) != "changed" {
		t.Fatal("apply failed")
	}
	// Automatic apply uses its background worker and durable socket URL too.
	_, logs = run("--on", "local", "--include-all", "--apply", "--", "/bin/sh", "-c", "printf automatic > file.txt")
	automatic := handleFrom(logs)
	status, _ := run("status", "--json", automatic)
	var st statusJSON
	if err := json.Unmarshal([]byte(status), &st); err != nil || st.AutomaticApply == nil || st.AutomaticApply.State != "applied" {
		t.Fatalf("automatic apply state: %s %v", status, err)
	}
	// Detached jobs can be found and attached from a fresh invocation.
	detached, _ := run("--on", "local", "--no-snapshot", "--no-apply", "-d", "--", "/bin/sh", "-c", "sleep 0.1; printf detached")
	dh := strings.TrimSpace(detached)
	if !strings.HasPrefix(dh, "local/") {
		t.Fatalf("detached handle: %s", detached)
	}
	run("attach", dh)
	run("status", "--json", dh)
	// Raw durable URLs work as handles without a process-local registration.
	id := strings.TrimPrefix(dh, "local/")
	if !proto.ValidULID(id) {
		t.Fatal(id)
	}
	run("status", target+"/"+id)
	// Persistent workspaces use the same local-only socket and owner identity.
	run("workspaces", "create", "--on", "local", "--include-all", "experiment")
	run("--on", "local", "--workspace", "experiment", "--no-apply", "--", "/bin/sh", "-c", "test \"$(cat file.txt)\" = automatic; printf persistent > file.txt")
	persistentOutput, _ := run("--on", "local", "--workspace", "experiment", "--no-apply", "--", "cat", "file.txt")
	if persistentOutput != "persistent" {
		t.Fatalf("persistent local contents: %q", persistentOutput)
	}
	if content, _ := os.ReadFile(filepath.Join(root, "file.txt")); string(content) != "automatic" {
		t.Fatal("persistent job changed local checkout")
	}
	run("workspaces", "rm", "--on", "local", "experiment")
	storage, _ := run("df", "--on", "local", "--json")
	var rows []dfRow
	if err := json.Unmarshal([]byte(storage), &rows); err != nil || len(rows) != 1 || rows[0].Location != "local" || rows[0].Changes == nil || rows[0].Changes.Bytes == 0 || rows[0].Jobs.Bytes == 0 {
		t.Fatalf("local df must combine jobs and fetched changes: %v %s", err, storage)
	}
	alias := filepath.Join(dir, "alias.sock")
	if err := os.Symlink(socket, alias); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(filepath.Dir(cfgPath), "config.toml")
	if err := os.WriteFile(clientPath, []byte(fmt.Sprintf("[peers.sandbox]\nsocket = %q\n", alias)), 0600); err != nil {
		t.Fatal(err)
	}
	// Fleet discovery includes both the alias and the built-in local target.
	// Querying both must report the same inventory as querying local alone.
	storage, _ = run("df", "--json")
	var aliasedRows []dfRow
	if err := json.Unmarshal([]byte(storage), &aliasedRows); err != nil || !reflect.DeepEqual(rows, aliasedRows) {
		t.Fatalf("socket alias changed local inventory: %v\nbefore: %+v\nafter: %s", err, rows, storage)
	}
	run("gc", "cache", "--on", "local", "--dry-run")
	run("gc", "jobs", "--on", "local", "--older-than", "7d", "--dry-run")
	run("gc", "all", "--on", "local", "--older-than", "7d", "--dry-run")
}
