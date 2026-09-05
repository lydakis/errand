package config

import (
	"github.com/BurntSushi/toml"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCachePrecedence(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := ClientPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("default_peer = 'runner'\n[peers.runner]\nurl = 'http://runner.invalid'\n[caches]\ncompiler = 'personal'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errand.toml"), []byte("[caches]\ncompiler = 'target'\n[profiles.clean.caches]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveRun(root, RunOverrides{})
	if err != nil || len(resolved.Caches) != 1 || resolved.Caches[0].Path != "target" {
		t.Fatalf("workspace caches: %+v %v", resolved, err)
	}
	resolved, err = ResolveRun(root, RunOverrides{Profile: "clean"})
	if err != nil || len(resolved.Caches) != 0 {
		t.Fatalf("clear caches: %+v %v", resolved, err)
	}
}

func TestRemovePeerPreservesCacheConfiguration(t *testing.T) {
	for _, caches := range []string{
		"[caches]\ncompiler = 'target'\n[profiles.clean.caches]\n[profiles.inherit.run]\nworkdir = '.'\n",
		"[caches]\n[profiles.build.caches]\n'go.build' = 'cache/go build'\n",
		"",
	} {
		t.Run(caches, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.toml")
			input := "default_peer = 'one'\n[peers.one]\nurl = 'http://one.invalid'\n[peers.two]\nurl = 'http://two.invalid'\n" + caches
			var before Client
			if _, err := toml.Decode(input, &before); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := RemovePeer(file, "one"); err != nil {
				t.Fatal(err)
			}
			var after Client
			if _, err := toml.DecodeFile(file, &after); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.Caches, after.Caches) {
				t.Fatalf("cache defaults changed: %+v", after.Caches)
			}
			for name, profile := range before.Profiles {
				if !reflect.DeepEqual(profile.Caches, after.Profiles[name].Caches) {
					t.Fatalf("profile %s cache override changed", name)
				}
			}
			if _, exists := after.Peers["one"]; exists || len(after.Peers) != 1 {
				t.Fatal("peer removal failed")
			}
		})
	}
}
