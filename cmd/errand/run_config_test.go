package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/daemon"
)

func TestConfigInspectionAndRunUseWorkspacePreferences(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	server := httptest.NewServer(d.Handler())
	t.Cleanup(server.Close)
	writeClientConfig(t, fmt.Sprintf("default_peer = 'missing'\napply_on_success = true\n[peers.test]\nurl = %q\n", server.URL))
	root := t.TempDir()
	t.Chdir(root)
	marker := "[run]\npeer = 'test'\n[changes]\napply_on_success = false\n"
	if err := os.WriteFile(".errand.toml", []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("report.txt", []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := cmdConfigTo([]string{"--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("config exit %d: %s", code, &stderr)
	}
	var got config.EffectiveRun
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Peer != "test" || got.URL != server.URL || got.ApplyOnSuccess || !strings.Contains(got.Sources["peer"], ".errand.toml") {
		t.Fatalf("inspection = %+v", got)
	}
	for _, apply := range []bool{false, true} {
		args := []string{"--include-all"}
		if apply {
			args = append(args, "--apply")
		}
		args = append(args, "--", "/bin/sh", "-c", "printf changed > report.txt")
		if code := cmdRun(args); code != 0 {
			t.Fatalf("run apply=%t exit %d", apply, code)
		}
		content, err := os.ReadFile("report.txt")
		want := "original"
		if apply {
			want = "changed"
		}
		if err != nil || string(content) != want {
			t.Fatalf("apply=%t content=%q, %v", apply, content, err)
		}
	}
}

func TestConfigExplicitFalseAndUsageErrors(t *testing.T) {
	writeClientConfig(t, "apply_on_success = true\ndefault_peer = 'test'\n[peers.test]\nurl = 'http://runner.invalid'\n")
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{"--apply=false"}, {"--no-apply"}} {
		var stdout, stderr bytes.Buffer
		if code := cmdConfigTo(append(args, "--json"), &stdout, &stderr); code != 0 {
			t.Fatalf("config %v: %d %s", args, code, &stderr)
		}
		var got config.EffectiveRun
		if err := json.Unmarshal(stdout.Bytes(), &got); err != nil || got.ApplyOnSuccess || !strings.HasPrefix(got.Sources["apply_on_success"], "cli:") {
			t.Fatalf("explicit false = %+v, %v", got, err)
		}
	}
	for _, args := range [][]string{
		{"--apply=false", "--no-apply"}, {"--on", "test", "--url", "http://other"},
		{"--on="}, {"--url="}, {"--no-snapshot", "-w", "child"},
		{"--no-snapshot", "--workspace-root", "."}, {"unexpected"}, {"--profile", "future"},
	} {
		var stdout, stderr bytes.Buffer
		if code := cmdConfigTo(args, &stdout, &stderr); code != 2 || stdout.Len() != 0 {
			t.Fatalf("config %v = %d, stdout=%q", args, code, &stdout)
		}
	}
}

func TestConfigEntryPointDoesNotResumeAutomaticApplies(t *testing.T) {
	if os.Getenv("ERRAND_CONFIG_ENTRYPOINT_TEST") == "1" {
		os.Args = []string{"errand", "config", "--json"}
		main()
		return
	}
	writeClientConfig(t, "default_peer = 'test'\n[peers.test]\nurl = 'http://runner.invalid'\n")
	t.Chdir(t.TempDir())
	// Resuming applications would reject this invalid state root and emit a
	// diagnostic. Inspection must never enter that path.
	t.Setenv("XDG_STATE_HOME", "relative-state-root")
	command := exec.Command(os.Args[0], "-test.run=^TestConfigEntryPointDoesNotResumeAutomaticApplies$")
	command.Env = append(os.Environ(), "ERRAND_CONFIG_ENTRYPOINT_TEST=1")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil || stderr.Len() != 0 {
		t.Fatalf("inspection: %v, stderr=%q", err, &stderr)
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("invalid config JSON: %q", &stdout)
	}
	if _, err := os.Stat("relative-state-root"); !os.IsNotExist(err) {
		t.Fatalf("inspection touched client state: %v", err)
	}
}
