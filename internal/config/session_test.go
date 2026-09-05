package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestSessionForwardPrecedence(t *testing.T) {
	root := runFixture(t, personalPeers+"\n[session]\nforward = ['3000']\n[profiles.dev.session]\nforward = ['8080:3000']\n", "[session]\nforward = ['4000']\n")
	for _, tc := range []struct {
		profile string
		cli     []string
		want    []string
		source  string
	}{
		{"", nil, []string{"4000"}, "workspace:"},
		{"dev", nil, []string{"8080:3000"}, "profiles.dev"},
		{"dev", []string{"9000"}, []string{"9000"}, "cli:"},
		{"dev", []string{}, []string{}, "cli:"},
	} {
		got, err := ResolveRun(root, RunOverrides{Profile: tc.profile, Forwards: tc.cli})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Forwards, tc.want) || !strings.Contains(got.Sources["forward"], tc.source) {
			t.Fatalf("forward preferences: %+v", got)
		}
	}
}

func TestSessionAttachmentDoesNotResolveRunTargetOrEnvironment(t *testing.T) {
	root := runFixture(t, "[profiles.dev.run]\npeer = 'absent'\nworkdir = 'build'\n[profiles.dev.env]\npass = ['ERRAND_UNSET_SESSION_TEST']\n[profiles.dev.session]\nforward = ['8080:3000']\n", "")
	got, err := ResolveSession(root, "dev", nil)
	if err != nil || !reflect.DeepEqual(got.Forwards, []string{"8080:3000"}) {
		t.Fatalf("session required unrelated run settings: %+v, %v", got, err)
	}
	if _, err := ResolveSession(root, "missing", nil); err == nil {
		t.Fatal("unknown attachment profile accepted")
	}
}

func TestSessionProfileReplacementAndPersistence(t *testing.T) {
	root := runFixture(t, personalPeers+"\n[session]\nforward = ['3000']\n[profiles.dev.session]\nforward = ['4000']\n[profiles.clear.session]\nforward = []\n[profiles.inherit.run]\npeer = 'linux'\n", "[profiles.dev.run]\npeer = 'linux'\n")
	path, _ := ClientPath()
	if _, err := RemovePeer(path, "mac"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		profile string
		want    []string
	}{
		{"dev", []string{"3000"}}, {"clear", []string{}}, {"inherit", []string{"3000"}},
	} {
		got, err := ResolveRun(root, RunOverrides{Profile: tc.profile})
		if err != nil || !reflect.DeepEqual(got.Forwards, tc.want) {
			t.Fatalf("profile %s after rewrite: %+v, %v", tc.profile, got, err)
		}
	}
}

func TestSessionSchemaRejectsInvalidMappings(t *testing.T) {
	for _, body := range []string{
		"forward = '3000'", "forward = [3000]", "forward = ['0']", "forward = ['65536']",
		"forward = ['localhost:3000']", "forward = ['3000', '3000:4000']", "forwards = ['3000']",
	} {
		for _, location := range []string{"personal", "workspace", "profile"} {
			t.Run(location+body, func(t *testing.T) {
				personal, project := personalPeers, ""
				switch location {
				case "personal":
					personal += "\n[session]\n" + body
				case "workspace":
					project = "[session]\n" + body
				case "profile":
					personal += "\n[profiles.dev.session]\n" + body
				}
				root := runFixture(t, personal, project)
				if _, err := ResolveRun(root, RunOverrides{}); err == nil {
					t.Fatal("invalid session accepted")
				}
			})
		}
	}
}
