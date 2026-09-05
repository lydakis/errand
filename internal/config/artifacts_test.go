package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestArtifactPrecedenceAndPersistence(t *testing.T) {
	root := runFixture(t, personalPeers+"\n[artifacts]\npaths = ['personal']\n[profiles.test.artifacts]\npaths = ['profile']\n[profiles.inherit.run]\npeer = 'linux'\n[profiles.replace.artifacts]\npaths = ['shadowed']\n", "[artifacts]\npaths = ['workspace']\n[profiles.clear.artifacts]\npaths = []\n[profiles.replace]\n")
	configPath, _ := ClientPath()
	if _, err := RemovePeer(configPath, "mac"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		profile   string
		cli, want []string
		source    string
	}{
		{"", nil, []string{"workspace"}, "workspace:"},
		{"test", nil, []string{"profile"}, "profiles.test"},
		{"clear", nil, []string{}, "profiles.clear"},
		{"inherit", nil, []string{"workspace"}, "workspace:"},
		{"replace", nil, []string{"workspace"}, "workspace:"},
		{"test", []string{"cli"}, []string{"cli"}, "cli:"},
		{"test", []string{}, []string{}, "cli:"},
	} {
		got, err := ResolveRun(root, RunOverrides{Profile: tc.profile, Artifacts: tc.cli})
		if err != nil || !reflect.DeepEqual(got.Artifacts, tc.want) || !strings.Contains(got.Sources["artifacts"], tc.source) {
			t.Fatalf("artifacts: %+v, %v", got, err)
		}
	}
}

func TestArtifactSchema(t *testing.T) {
	for _, body := range []string{"paths = 'dist'", "paths = [1]", "path = ['dist']", "paths = ['../dist']", "paths = ['.git']", "paths = ['dist/*']"} {
		for _, section := range []string{"artifacts", "profiles.test.artifacts"} {
			root := runFixture(t, personalPeers, "["+section+"]\n"+body+"\n")
			if _, err := ResolveRun(root, RunOverrides{}); err == nil {
				t.Fatalf("accepted %s: %s", section, body)
			}
		}
	}
}
