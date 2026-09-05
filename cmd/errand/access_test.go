package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/daemon"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/tailnet"
)

func accessConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.toml")
	if err := os.WriteFile(path, []byte("# local runner\nallow_users = ['owner@example.com']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAccessCLI(t *testing.T) {
	path := accessConfig(t)
	var out, errOut bytes.Buffer
	run := func(args ...string) {
		t.Helper()
		out.Reset()
		errOut.Reset()
		if code := cmdAccessTo(args, &out, &errOut); code != 0 || errOut.Len() != 0 {
			t.Fatalf("%v: code=%d, stderr=%s", args, code, &errOut)
		}
	}
	run("add", "--config", path, "--json", "-n", "friend@example.com")
	var result struct {
		config.AccessChange
		DryRun     bool   `json:"dry_run"`
		Activation string `json:"activation"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || !result.Changed || result.Written || !reflect.DeepEqual(result.After, []string{"owner@example.com", "friend@example.com"}) || !strings.Contains(result.Activation, "restart") {
		t.Fatalf("preview: %+v", result)
	}
	run("--config", path, "--json")
	var policy config.AccessPolicy
	if err := json.Unmarshal(out.Bytes(), &policy); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(policy.AllowUsers, []string{"owner@example.com"}) || policy.Path != path {
		t.Fatalf("list after preview: %+v", policy)
	}
	run("add", "--config", path, "friend@example.com")
	for _, want := range []string{"Updated", "restart", "Capability grants and SSH", "removes comments", "errand setup --config"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %s", want, &out)
		}
	}
	run("remove", "--config", path, "--json", "friend@example.com")
	result = struct {
		config.AccessChange
		DryRun     bool   `json:"dry_run"`
		Activation string `json:"activation"`
	}{}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Written || !result.Changed || !reflect.DeepEqual(result.After, []string{"owner@example.com"}) {
		t.Fatalf("remove: %+v", result)
	}
}

func TestAccessCLIUsage(t *testing.T) {
	for _, args := range [][]string{{"grant"}, {"add"}, {"remove", "a", "b"}, {"list", "a"}, {"list", "--on", "runner"}} {
		var out, errOut bytes.Buffer
		if code := cmdAccessTo(args, &out, &errOut); code != 2 || out.Len() != 0 {
			t.Fatalf("%v: code=%d, output=%s", args, code, &out)
		}
	}
	for _, action := range []string{"list", "add", "remove"} {
		var out, errOut bytes.Buffer
		if code := cmdAccessTo([]string{action, "--help"}, &out, &errOut); code != 0 {
			t.Fatalf("help %s: %d", action, code)
		}
	}
}

func TestAccessEntryPointDoesNotResumeAutomaticApplies(t *testing.T) {
	if os.Getenv("ERRAND_ACCESS_ENTRYPOINT_TEST") == "1" {
		os.Args = []string{"errand", "access", "--json"}
		main()
		return
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configDir := filepath.Join(root, ".config", "errand")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "errandd.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	t.Setenv("XDG_STATE_HOME", "relative-state-root")
	command := exec.Command(os.Args[0], "-test.run=^TestAccessEntryPointDoesNotResumeAutomaticApplies$")
	command.Env = append(os.Environ(), "ERRAND_ACCESS_ENTRYPOINT_TEST=1")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil || stderr.Len() != 0 || !json.Valid(stdout.Bytes()) {
		t.Fatalf("access: %v, stdout=%q, stderr=%q", err, &stdout, &stderr)
	}
	if _, err := os.Stat("relative-state-root"); !os.IsNotExist(err) {
		t.Fatalf("access touched client state: %v", err)
	}
}

type accessIdentityProvider struct {
	tailnet.Provider
	capability bool
}

func (p accessIdentityProvider) WhoIs(context.Context, string, string) (tailnet.WhoIs, error) {
	id := tailnet.WhoIs{LoginName: "friend@example.com", UserID: 42, NodeStableID: "test-friend"}
	if p.capability {
		id.CapMap = map[string][]json.RawMessage{proto.DefaultCapability: {json.RawMessage(`{"actions":["submit"]}`)}}
	}
	return id, nil
}

func TestAccessEditsTakeEffectOnlyAfterRunnerReload(t *testing.T) {
	path := accessConfig(t)
	start := func(capability bool) *httptest.Server {
		t.Helper()
		policy, err := config.ReadAccess(path)
		if err != nil {
			t.Fatal(err)
		}
		d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), AllowUsers: policy.AllowUsers, Capability: policy.Capability, Identity: accessIdentityProvider{capability: capability}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := d.Close(); err != nil {
				t.Error(err)
			}
		})
		s := httptest.NewServer(d.Handler())
		t.Cleanup(s.Close)
		return s
	}
	check := func(s *httptest.Server, want int) {
		t.Helper()
		response, err := s.Client().Get(s.URL + "/v0/info")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("runner response=%d, want %d", response.StatusCode, want)
		}
	}
	edit := func(action string) {
		t.Helper()
		var out, errOut bytes.Buffer
		if code := cmdAccessTo([]string{action, "--config", path, "friend@example.com"}, &out, &errOut); code != 0 {
			t.Fatalf("edit: %d, %s", code, &errOut)
		}
	}
	original := start(false)
	check(original, http.StatusForbidden)
	edit("add")
	check(original, http.StatusForbidden)
	added := start(false)
	check(added, http.StatusOK)
	edit("remove")
	check(added, http.StatusOK)
	check(start(false), http.StatusForbidden)
	// Removing the allowlist entry does not revoke an independent grant.
	check(start(true), http.StatusOK)
}
