package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
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

func TestEnvironmentInspectionAndDoctorHideValues(t *testing.T) {
	t.Setenv("ERRAND_TEST_PASS", "dummy-forwarded-value")
	writeClientConfig(t, "default_peer = 'test'\n[peers.test]\nurl = 'http://runner.invalid'\n[env]\npass = ['ERRAND_TEST_PASS']\n")
	t.Chdir(t.TempDir())
	for _, asJSON := range []bool{false, true} {
		args := []string{"--env", "CI=dummy-literal-value"}
		if asJSON {
			args = append(args, "--json")
		}
		var out, errOut bytes.Buffer
		if code := cmdConfigTo(args, &out, &errOut); code != 0 {
			t.Fatalf("config code=%d: %s", code, &errOut)
		}
		if strings.Contains(out.String()+errOut.String(), "dummy-") {
			t.Fatal("config leaked a value")
		}
		if !strings.Contains(out.String(), "ERRAND_TEST_PASS") || !strings.Contains(out.String(), "CI") {
			t.Fatal("config omitted environment metadata")
		}
	}
	var out, errOut bytes.Buffer
	code := cmdDoctorTo([]string{"--json", "--env", "CI=dummy-literal-value"}, &out, &errOut, func(context.Context, string) (proto.Info, error) { return proto.Info{Version: "test"}, nil })
	if code != 0 || strings.Contains(out.String()+errOut.String(), "dummy-") {
		t.Fatal("doctor failed or leaked values")
	}
	// Missing and empty are distinct: an empty but set variable is available.
	t.Setenv("ERRAND_TEST_PASS", "")
	out.Reset()
	errOut.Reset()
	if code := cmdConfigTo([]string{"--json"}, &out, &errOut); code != 0 {
		t.Fatal("empty variable rejected")
	}
	var effective config.EffectiveRun
	if err := json.Unmarshal(out.Bytes(), &effective); err != nil || len(effective.MissingEnvironment()) != 0 {
		t.Fatal("empty variable marked unavailable")
	}
}

func TestMissingEnvironmentStopsDoctorAndSubmission(t *testing.T) {
	writeClientConfig(t, "default_peer = 'test'\n[peers.test]\nurl = 'http://runner.invalid'\n[env]\npass = ['ERRAND_TEST_MISSING_24619']\n")
	t.Chdir(t.TempDir())
	t.Setenv("XDG_STATE_HOME", "invalid-state-root")
	var out, errOut bytes.Buffer
	code := cmdDoctorTo([]string{"--json"}, &out, &errOut, func(context.Context, string) (proto.Info, error) {
		t.Fatal("doctor probed despite missing env")
		return proto.Info{}, nil
	})
	if code != 1 || !strings.Contains(out.String(), "ERRAND_TEST_MISSING_24619") || !strings.Contains(out.String(), "skipped") {
		t.Fatalf("doctor: %d %s", code, &out)
	}
	// The direct client must stop before state creation or any network request.
	if code := client.Run(client.RunOptions{PeerURL: "invalid://runner", Root: "/missing", Argv: []string{"true"}, PassEnv: []string{"ERRAND_TEST_MISSING_24619"}, Stdout: &out, Stderr: &errOut}); code != client.ExitTransaction || !strings.Contains(errOut.String(), "required environment variable") {
		t.Fatalf("submission: %d %s", code, &errOut)
	}
	if _, err := os.Stat("invalid-state-root"); !os.IsNotExist(err) {
		t.Fatal("failed environment check touched state")
	}
}

func TestWorkspacePassStopsDoctorAndRunBeforeContact(t *testing.T) {
	writeClientConfig(t, "default_peer = 'test'\n[peers.test]\nurl = 'http://runner.invalid'\n")
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".errand.toml", []byte("[env]\npass = ['ERRAND_CONSENT_TEST']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ERRAND_CONSENT_TEST", "dummy-value")
	t.Setenv("XDG_STATE_HOME", "invalid-state-root")
	var out, errOut bytes.Buffer
	code := cmdDoctorTo([]string{"--json"}, &out, &errOut, func(context.Context, string) (proto.Info, error) {
		t.Fatal("doctor contacted the runner with an invalid workspace forwarding default")
		return proto.Info{}, nil
	})
	if code != 1 || !strings.Contains(out.String(), "workspace env.pass") || strings.Contains(out.String(), "dummy-value") {
		t.Fatalf("doctor: %d %s", code, &out)
	}
	if code := cmdRun([]string{"--no-snapshot", "--", "true"}); code != client.ExitTransaction {
		t.Fatalf("run accepted workspace env.pass: %d", code)
	}
	if _, err := os.Stat("invalid-state-root"); !os.IsNotExist(err) {
		t.Fatal("invalid workspace policy touched submission state")
	}
}

func TestConfiguredEnvironmentReachesJobWithoutPersistingValues(t *testing.T) {
	t.Setenv("ERRAND_TEST_PASS", "dummy-forwarded-value")
	t.Setenv("ERRAND_TEST_INACTIVE", "dummy-inactive-value")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	state := t.TempDir()
	d, err := daemon.New(daemon.Config{StateDir: state, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	server := httptest.NewServer(d.Handler())
	t.Cleanup(server.Close)
	writeClientConfig(t, fmt.Sprintf("default_peer = 'test'\n[peers.test]\nurl = %q\n[env]\nset = { KEEP = 'yes' }\n[profiles.inactive.env]\npass = ['ERRAND_TEST_INACTIVE']\n", server.URL))
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".errand.toml", []byte("[profiles.integration.env]\nset = { CI = 'workspace' }\npass = ['ERRAND_TEST_PASS']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := cmdRun([]string{"--profile", "integration", "--env", "CI=cli", "--no-snapshot", "--", "/bin/sh", "-c", `test "$CI" = cli && test "$KEEP" = yes && test -n "$ERRAND_TEST_PASS" && test -z "${ERRAND_TEST_INACTIVE+x}"`})
	if code != 0 {
		t.Fatalf("environment job exit=%d", code)
	}
	specs := 0
	err = filepath.WalkDir(state, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte("dummy-forwarded-value")) || bytes.Contains(data, []byte("dummy-inactive-value")) {
			t.Fatal("receipt persisted a value")
		}
		if entry.Name() == "spec.json" {
			specs++
			var receipt proto.ReceiptSpec
			if err := json.Unmarshal(data, &receipt); err != nil {
				return err
			}
			if receipt.EnvSources["ERRAND_TEST_PASS"] != "passenv" || receipt.EnvSources["CI"] != "literal" {
				t.Fatal("receipt lost environment provenance")
			}
		}
		return nil
	})
	if err != nil || specs != 1 {
		t.Fatalf("receipt validation: specs=%d err=%v", specs, err)
	}
}
