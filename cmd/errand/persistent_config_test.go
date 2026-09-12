package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
)

func TestPersistentConfigRejectsInvalidExecution(t *testing.T) {
	writeClientConfig(t, "default_peer = 'test'\n[peers.test]\nurl = 'http://runner.invalid'\n[profiles.dev.run]\nworkspace = 'development'\n")
	t.Chdir(t.TempDir())
	for _, flags := range [][]string{{"--no-snapshot"}, {"--where", "*"}} {
		var out, stderr bytes.Buffer
		args := append([]string{"--profile", "dev", "--json"}, flags...)
		if code := cmdConfigTo(args, &out, &stderr); code != 2 || out.Len() != 0 || !strings.Contains(stderr.String(), "persistent workspace") {
			t.Fatalf("config %v: %d, %s, %s", flags, code, &out, &stderr)
		}
		out.Reset()
		if code := cmdDoctorTo(args, &out, &stderr, func(context.Context, string) (proto.Info, error) {
			t.Fatal("probed an invalid persistent execution configuration")
			return proto.Info{}, nil
		}); code != 1 {
			t.Fatalf("doctor %v: %d %s", flags, code, &out)
		}
		var report doctorReport
		if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.OK || report.Effective != nil || !strings.Contains(out.String(), "persistent workspace") {
			t.Fatalf("doctor report: %s, %v", &out, err)
		}
	}
}

func TestPersistentConfigDistinguishesInheritedAndExplicitSettings(t *testing.T) {
	writeClientConfig(t, "default_peer = 'test'\n[peers.test]\nurl = 'http://runner.invalid'\n[profiles.dev.run]\nworkspace = 'development'\n[profiles.explicit.run]\nworkspace = 'development'\n[profiles.explicit.caches]\nbuild = 'target'\n[profiles.explicit.artifacts]\npaths = ['reports']\n")
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".errand.toml", []byte("[caches]\nambient = 'ambient-cache'\n[artifacts]\npaths = ['ambient-output']\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                    string
		args                    []string
		inherited               bool
		cachePath, artifactPath string
	}{
		{"inherited", []string{"--profile", "dev"}, true, "", ""},
		{"profile overrides", []string{"--profile", "explicit"}, false, "target", "reports"},
		{"explicit empty", []string{"--profile", "dev", "--no-caches", "--no-artifacts"}, false, "", ""},
		{"ephemeral", nil, false, "ambient-cache", "ambient-output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			if code := cmdConfigTo(append(tc.args, "--json"), &out, &stderr); code != 0 {
				t.Fatalf("config: %d %s", code, &stderr)
			}
			var got config.EffectiveRun
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"caches", "artifacts"} {
				if strings.Contains(got.Sources[key], "resolved on runner") != tc.inherited {
					t.Fatalf("%s source: %q", key, got.Sources[key])
				}
			}
			if tc.inherited {
				if got.Caches != nil || got.Artifacts != nil {
					t.Fatalf("reported ambient values: %+v", got)
				}
			} else if got.Caches == nil || got.Artifacts == nil {
				t.Fatalf("lost explicit or ephemeral settings: %+v", got)
			}
			if tc.cachePath == "" {
				if len(got.Caches) != 0 || len(got.Artifacts) != 0 {
					t.Fatalf("unexpected cache/artifact settings: %+v", got)
				}
			} else if len(got.Caches) != 1 || got.Caches[0].Path != tc.cachePath || len(got.Artifacts) != 1 || got.Artifacts[0] != tc.artifactPath {
				t.Fatalf("wrong cache/artifact settings: %+v", got)
			}
			out.Reset()
			if code := cmdConfigTo(tc.args, &out, &stderr); code != 0 {
				t.Fatalf("human config: %d %s", code, &stderr)
			}
			if tc.inherited && (strings.Contains(out.String(), "ambient-cache") || strings.Contains(out.String(), "ambient-output") || !strings.Contains(out.String(), "resolved on runner")) {
				t.Fatalf("misleading human output: %s", &out)
			}
		})
	}
}
