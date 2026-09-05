package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func accessFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAccessEditsPreserveOtherRunnerSettings(t *testing.T) {
	body := "# runner\nlisten = 'none'\nsocket = '/tmp/custom.sock'\nallow_users = ['owner@example.com']\ncapability = 'example.com/runner'\nmax_jobs = 3\n[cache]\nmax_bytes = 1234\n[future]\nfeature = true\n"
	path := accessFixture(t, body)
	var before map[string]any
	if _, err := toml.Decode(body, &before); err != nil {
		t.Fatal(err)
	}
	change, err := ChangeAccess(path, "friend@example.com", true, false)
	if err != nil || !change.Changed || !change.Written {
		t.Fatalf("add: %+v, %v", change, err)
	}
	if !reflect.DeepEqual(change.After, []string{"owner@example.com", "friend@example.com"}) {
		t.Fatalf("users: %v", change.After)
	}
	var after map[string]any
	if _, err := toml.DecodeFile(path, &after); err != nil {
		t.Fatal(err)
	}
	delete(before, "allow_users")
	delete(after, "allow_users")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("unrelated config changed: %v vs %v", before, after)
	}
	change, err = ChangeAccess(path, "owner@example.com", false, false)
	if err != nil || !reflect.DeepEqual(change.After, []string{"friend@example.com"}) {
		t.Fatalf("remove: %+v, %v", change, err)
	}
	policy, err := ReadAccess(path)
	if err != nil || policy.Capability != "example.com/runner" || policy.Listen != "none" {
		t.Fatalf("policy: %+v, %v", policy, err)
	}
}

func TestAccessDryRunAndNoOpPreserveExactFile(t *testing.T) {
	body := "# keep formatting\nallow_users=['owner@example.com']\n"
	path := accessFixture(t, body)
	for _, tc := range []struct {
		login             string
		add, dry, changed bool
	}{
		{"friend@example.com", true, true, true},
		{"owner@example.com", false, true, true},
		{"owner@example.com", true, false, false},
		{"missing@example.com", false, false, false},
	} {
		change, err := ChangeAccess(path, tc.login, tc.add, tc.dry)
		if err != nil || change.Written || change.Changed != tc.changed {
			t.Fatalf("change: %+v, %v", change, err)
		}
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != body {
			t.Fatalf("preview/no-op changed file: %q, %v", raw, err)
		}
	}
}

func TestAccessRemoveClearsDuplicatesAndLastEntry(t *testing.T) {
	path := accessFixture(t, "allow_users = ['owner@example.com', 'owner@example.com']\n")
	change, err := ChangeAccess(path, "owner@example.com", false, false)
	if err != nil || len(change.After) != 0 || !change.Written {
		t.Fatalf("remove duplicates: %+v, %v", change, err)
	}
	policy, err := ReadAccess(path)
	if err != nil || len(policy.AllowUsers) != 0 {
		t.Fatalf("last entry remained: %+v, %v", policy, err)
	}
}

func TestAccessAddsToEmptyConfig(t *testing.T) {
	path := accessFixture(t, "")
	change, err := ChangeAccess(path, "owner@example.com", true, false)
	if err != nil || !change.Written || len(change.Before) != 0 {
		t.Fatalf("empty config: %+v, %v", change, err)
	}
}

func TestAccessDefaultsToSetupPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	setupPath := filepath.Join(home, ".config", "errand", "errandd.toml")
	xdgPath := filepath.Join(xdg, "errand", "errandd.toml")
	body := "allow_users = ['owner@example.com']\n"
	for _, path := range []string{setupPath, xdgPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := ReadAccess("")
	if err != nil || policy.Path != setupPath {
		t.Fatalf("default must match setup: %+v, %v", policy, err)
	}
	for _, add := range []bool{true, false} {
		change, err := ChangeAccess("", "friend@example.com", add, false)
		if err != nil || change.Path != setupPath || !change.Written {
			t.Fatalf("default edit: %+v, %v", change, err)
		}
	}
	raw, err := os.ReadFile(xdgPath)
	if err != nil || string(raw) != body {
		t.Fatalf("default edit touched XDG config: %q, %v", raw, err)
	}
	// An explicitly selected XDG file remains supported.
	change, err := ChangeAccess(xdgPath, "friend@example.com", true, false)
	if err != nil || change.Path != xdgPath || !change.Written {
		t.Fatalf("explicit edit: %+v, %v", change, err)
	}
}

func TestAccessRefusesInvalidInputsWithoutWriting(t *testing.T) {
	path := accessFixture(t, "allow_users = ['owner@example.com']\n")
	for _, login := range []string{"", "*", " friend@example.com", "friend@example.com\n", "friend name"} {
		if _, err := ChangeAccess(path, login, true, false); err == nil {
			t.Fatalf("accepted login %q", login)
		}
	}
	for _, body := range []string{"[broken", "allow_users = 'owner@example.com'"} {
		path := accessFixture(t, body)
		if _, err := ChangeAccess(path, "friend@example.com", true, false); err == nil {
			t.Fatal("accepted invalid config")
		}
		raw, _ := os.ReadFile(path)
		if string(raw) != body {
			t.Fatal("invalid file was overwritten")
		}
	}
	missing := filepath.Join(t.TempDir(), "missing.toml")
	if _, err := ChangeAccess(missing, "friend@example.com", true, false); err == nil || !strings.Contains(err.Error(), "setup") {
		t.Fatalf("missing config: %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("created missing runner config")
	}
	link := filepath.Join(t.TempDir(), "link.toml")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ChangeAccess(link, "friend@example.com", true, false); err == nil {
		t.Fatal("replaced symlink")
	}
}
