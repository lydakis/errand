package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEnvironmentFilesParseWithoutExpansion(t *testing.T) {
	root := runFixture(t, personalPeers, "[profiles.dev.env]\nfiles = ['.env.local']\n")
	body := "# comment\r\nexport PLAIN = value # comment\r\nEMPTY=\nSINGLE='literal $HOME # text'\nDOUBLE=\"line\\nnext\\t\\\"quoted\\\"\"\nMULTI='first  \nsecond'\nRAW=$(touch should-not-exist)\nHASH=value#fragment\nDUP=first\nDUP=last\n"
	if err := os.WriteFile(filepath.Join(root, ".env.local"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePreparedRun(root, RunOverrides{Profile: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	values, pass := got.JobEnvironment()
	want := map[string]string{"PLAIN": "value", "EMPTY": "", "SINGLE": "literal $HOME # text", "DOUBLE": "line\nnext\t\"quoted\"", "MULTI": "first  \nsecond", "RAW": "$(touch should-not-exist)", "HASH": "value#fragment", "DUP": "last"}
	if !reflect.DeepEqual(values, want) || len(pass) != 0 {
		t.Fatal("file values did not preserve the documented syntax")
	}
	if _, err := os.Stat(filepath.Join(root, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatal("executed file content")
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "touch should-not-exist") || strings.Contains(string(raw), "literal $HOME") {
		t.Fatal("diagnostics leaked file values")
	}
	for _, entry := range got.Environment {
		if entry.Kind != "file" || !strings.Contains(entry.Source, ".env.local") || !entry.Available {
			t.Fatal("missing file provenance")
		}
	}
}

func TestEnvironmentFilesLayeringAndRelativePaths(t *testing.T) {
	root := runFixture(t, personalPeers+"\n[env]\nfiles = ['missing-personal.env']\nset = { KEEP = 'personal', VALUE = 'personal' }\n", "[workspace]\nroot = true\n[profiles.dev.env]\nfiles = ['base.env', 'override.env']\nset = { SET = 'explicit' }\npass = ['PASS']\n")
	t.Setenv("PASS", "ambient")
	for name, body := range map[string]string{"base.env": "VALUE=file\nSET=file\nPASS=file\nBASE=yes\n", "override.env": "VALUE=last\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePreparedRun(child, RunOverrides{Profile: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	values, pass := got.JobEnvironment()
	if !reflect.DeepEqual(values, map[string]string{"KEEP": "personal", "VALUE": "last", "SET": "explicit", "BASE": "yes"}) || !reflect.DeepEqual(pass, []string{"PASS"}) {
		t.Fatal("wrong file/literal/forwarding precedence")
	}
	// Unselected profiles and replaced file lists must never be read.
	if err := os.WriteFile(filepath.Join(root, ".errand.toml"), []byte("[env]\nfiles = []\n[profiles.inactive.env]\nfiles = ['missing-inactive.env']\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolvePreparedRun(root, RunOverrides{}); err != nil {
		t.Fatal(err)
	}
	// Personal-profile paths belong to the personal config directory.
	personalPath, _ := ClientPath()
	if err := os.WriteFile(filepath.Join(filepath.Dir(personalPath), "personal.env"), []byte("PERSONAL=yes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(personalPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("\n[profiles.personal.env]\nfiles = ['personal.env']\n")
	if closeErr := f.Close(); err != nil || closeErr != nil {
		t.Fatalf("append config: %v %v", err, closeErr)
	}
	got, err = resolvePreparedRun(root, RunOverrides{Profile: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	values, _ = got.JobEnvironment()
	if values["PERSONAL"] != "yes" {
		t.Fatal("wrong personal path base")
	}
}

func TestEnvironmentFileErrorsArePrivate(t *testing.T) {
	for _, body := range []string{"PRIVATE_SECRET", "PRIVATE_SECRET=value\x00", "KEY='PRIVATE_SECRET", "KEY=\"PRIVATE_SECRET\" trailing", "1PRIVATE_SECRET=value", strings.Repeat("x", (1<<20)+1)} {
		root := runFixture(t, personalPeers, "[profiles.dev.env]\nfiles = ['bad.env']\n")
		if err := os.WriteFile(filepath.Join(root, "bad.env"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := resolvePreparedRun(root, RunOverrides{Profile: "dev"})
		if err == nil || strings.Contains(err.Error(), "PRIVATE_SECRET") {
			t.Fatalf("missing or unredacted parse error: %v", err)
		}
	}
}

func TestEnvironmentFileConsentAndSchema(t *testing.T) {
	for _, body := range []string{"[env]\nfiles = ['missing.env']", "[profiles.dev.env]\nfiles = 'bad'", "[profiles.dev.env]\nfiles = [false]", "[profiles.dev.env]\nfiles = ['']"} {
		root := runFixture(t, personalPeers, body)
		if _, err := ResolveRun(root, RunOverrides{}); err == nil {
			t.Fatal("accepted invalid file selection")
		}
	}
}

func TestEnvironmentFileListsSurvivePeerRewrite(t *testing.T) {
	runFixture(t, personalPeers+"\n[env]\nfiles=['default.env']\n[profiles.clear.env]\nfiles=[]\n[profiles.inherit.env]\nset={CI='1'}\n", "")
	path, _ := ClientPath()
	if _, err := RemovePeer(path, "mac"); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Environment.Files, []string{"default.env"}) || cfg.Profiles["clear"].Environment.Files == nil || cfg.Profiles["inherit"].Environment.Files != nil {
		t.Fatalf("peer rewrite changed file-list inheritance: default=%v clearNil=%t inheritNil=%t", cfg.Environment.Files, cfg.Profiles["clear"].Environment.Files == nil, cfg.Profiles["inherit"].Environment.Files == nil)
	}
}

func TestEnvironmentPassReplacementPreservesDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, profile string
		want          map[string]string
		pass          []string
	}{
		{"clear", "pass=[]", map[string]string{"TOKEN": "file", "KEEP": "file", "LITERAL": "personal"}, nil},
		{"replace", "pass=['OTHER']", map[string]string{"TOKEN": "file", "KEEP": "file", "LITERAL": "personal"}, []string{"OTHER"}},
		{"literal", "pass=[]\nset={TOKEN='profile'}", map[string]string{"TOKEN": "profile", "KEEP": "file", "LITERAL": "personal"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := runFixture(t, personalPeers+"\n[env]\nfiles=['defaults.env']\nset={LITERAL='personal'}\npass=['TOKEN']\n", "[profiles.dev.env]\n"+tc.profile+"\n")
			path, _ := ClientPath()
			if err := os.WriteFile(filepath.Join(filepath.Dir(path), "defaults.env"), []byte("TOKEN=file\nKEEP=file\n"), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := resolvePreparedRun(root, RunOverrides{Profile: "dev"})
			if err != nil {
				t.Fatal(err)
			}
			values, pass := got.JobEnvironment()
			if !reflect.DeepEqual(values, tc.want) || !reflect.DeepEqual(pass, tc.pass) {
				t.Fatal("replacing forwarding lost defaults or changed literal precedence")
			}
		})
	}
}
