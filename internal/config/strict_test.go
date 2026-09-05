package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigRejectsUnknownSettings(t *testing.T) {
	for _, tc := range []struct {
		name, body, key string
		daemon          bool
	}{
		{"personal", "apply_on_sucess = true", "apply_on_sucess", false},
		{"peer", "[peers.build]\nurl = 'http://build:7443'\nremote_comand = '/bin/errand'", "peers.build.remote_comand", false},
		{"runner", "max_job = 2", "max_job", true},
		{"runner cache", "[named_cache]\nmax_byte = 10", "named_cache.max_byte", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", root)
			dir := filepath.Join(root, "errand")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			name := "config.toml"
			if tc.daemon {
				name = "errandd.toml"
			}
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			if tc.daemon {
				_, err = LoadDaemon("")
			} else {
				_, err = LoadClient()
			}
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("error = %v, want file and unknown key %s", err, tc.key)
			}
		})
	}
}

func TestPeerEditsRefuseUnknownSettingsWithoutRewriting(t *testing.T) {
	body := "apply_on_sucess = true\n[peers.build]\nurl = 'http://build:7443'\n"
	for _, edit := range []struct {
		name string
		run  func(string) error
	}{
		{"add", func(path string) error {
			_, err := AddPeer(path, "other", Peer{URL: "http://other:7443"}, false)
			return err
		}},
		{"remove", func(path string) error { _, err := RemovePeer(path, "build"); return err }},
	} {
		t.Run(edit.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := edit.run(path); err == nil || !strings.Contains(err.Error(), "apply_on_sucess") {
				t.Fatalf("error = %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != body {
				t.Fatalf("config changed: %s, %v", got, err)
			}
		})
	}
}
