package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/workspace"
)

func TestEnvironmentPrecedenceAndRedaction(t *testing.T) {
	t.Setenv("ERRAND_TEST_PASS", "dummy-forwarded-value")
	root := runFixture(t, personalPeers+"\n[env]\nset = { CI = 'personal', KEEP = 'yes' }\npass = ['OLD']\n[profiles.build.env]\nset = { CI = 'profile', TO_PASS = 'old' }\npass = ['ERRAND_TEST_PASS']\n", "[env]\nset = { CI = 'workspace' }\n")
	got, err := ResolveRun(root, RunOverrides{Profile: "build", Environment: workspace.Environment{Set: map[string]string{"CI": "dummy-literal-value"}, Pass: []string{"TO_PASS"}}})
	if err != nil {
		t.Fatal(err)
	}
	literals, pass := got.JobEnvironment()
	if !reflect.DeepEqual(literals, map[string]string{"CI": "dummy-literal-value", "KEEP": "yes"}) || !reflect.DeepEqual(pass, []string{"TO_PASS"}) {
		t.Fatalf("wrong precedence or pass-list replacement")
	}
	for _, entry := range got.Environment {
		if entry.Name == "CI" && !strings.HasPrefix(entry.Source, "cli:") {
			t.Fatal("missing CLI provenance")
		}
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "dummy-literal-value") || strings.Contains(string(raw), "dummy-forwarded-value") {
		t.Fatal("environment value leaked in JSON")
	}
	got, err = ResolveRun(root, RunOverrides{Profile: "build"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.MissingEnvironment()) != 0 {
		t.Fatal("available variable reported missing")
	}
	_, pass = got.JobEnvironment()
	if !reflect.DeepEqual(pass, []string{"ERRAND_TEST_PASS"}) {
		t.Fatal("profile forwarding did not replace defaults")
	}
}

func TestEnvironmentEmptyPassAndProfileReplacement(t *testing.T) {
	root := runFixture(t, personalPeers+"\n[env]\npass = ['ERRAND_TEST_REQUIRED']\n[profiles.build.env]\nset = { ONLY_IN_PERSONAL_PROFILE = 'yes' }\n", "[profiles.build.env]\npass = []\n")
	got, err := ResolveRun(root, RunOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.MissingEnvironment(), []string{"ERRAND_TEST_REQUIRED"}) {
		t.Fatal("inactive profile changed required variables")
	}
	got, err = ResolveRun(root, RunOverrides{Profile: "build"})
	if err != nil {
		t.Fatal(err)
	}
	literal, pass := got.JobEnvironment()
	if len(literal) != 0 || len(pass) != 0 {
		t.Fatal("empty profile pass list or whole-profile replacement was lost")
	}
}

func TestEnvironmentSchemaRejectsInvalidValues(t *testing.T) {
	for _, body := range []string{
		"[env]\nset = { CI = 1 }", "[env]\npass = 'CI'", "[env]\npass = [1]",
		"[env]\nunknown = 'dummy-value'", "[env]\nset = { 'A=B' = 'dummy-value' }",
		"[env]\nset = { CI = 'dummy-value' }\npass = ['CI']",
	} {
		root := runFixture(t, personalPeers, body)
		if _, err := ResolveRun(root, RunOverrides{}); err == nil {
			t.Fatalf("accepted invalid environment schema")
		} else if strings.Contains(err.Error(), "dummy-value") {
			t.Fatal("schema error leaked literal")
		}
	}
}

func TestEnvironmentConfigSurvivesPeerRewrite(t *testing.T) {
	runFixture(t, personalPeers+"\n[env]\nset = { CI = '1' }\npass = []\n[profiles.build.env]\npass = ['ERRAND_TEST_PASS']\n[profiles.literal.env]\nset = { CI = '2' }\n", "")
	path, _ := ClientPath()
	if _, err := RemovePeer(path, "mac"); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Environment.Set["CI"] != "1" || cfg.Environment.Pass == nil || !reflect.DeepEqual(cfg.Profiles["build"].Environment.Pass, []string{"ERRAND_TEST_PASS"}) {
		t.Fatal("peer rewrite lost environment settings")
	}
	if cfg.Profiles["literal"].Environment.Pass != nil {
		t.Fatal("peer rewrite turned inherited forwarding into an empty list")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceEnvironmentRequiresExplicitForwardingChoice(t *testing.T) {
	t.Setenv("ERRAND_CONSENT_TEST", "dummy-value")
	root := runFixture(t, personalPeers+"\n[env]\npass = ['ERRAND_CONSENT_TEST']\n", "[env]\npass = ['ERRAND_CONSENT_TEST']\n")
	if _, err := ResolveRun(root, RunOverrides{}); err == nil || !strings.Contains(err.Error(), "workspace env.pass") {
		t.Fatalf("workspace must not select ambient values: %v", err)
	}
	// Explicit CLI choices must not accidentally hide an invalid workspace
	// default that would become active again on the next ordinary invocation.
	if _, err := ResolveRun(root, RunOverrides{Environment: workspace.Environment{Pass: []string{"ERRAND_CONSENT_TEST"}}}); err == nil {
		t.Fatal("CLI override hid an invalid workspace forwarding default")
	}
	project := "[env]\npass = []\n[profiles.integration.env]\npass = ['ERRAND_CONSENT_TEST']\n"
	if err := os.WriteFile(filepath.Join(root, ".errand.toml"), []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRun(root, RunOverrides{Profile: "integration"})
	if err != nil {
		t.Fatal(err)
	}
	_, pass := got.JobEnvironment()
	if !reflect.DeepEqual(pass, []string{"ERRAND_CONSENT_TEST"}) {
		t.Fatal("explicit profile lost its forwarding choice")
	}
	got, err = ResolveRun(root, RunOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	_, pass = got.JobEnvironment()
	if len(pass) != 0 {
		t.Fatal("empty workspace pass list did not clear inherited forwarding")
	}
}
