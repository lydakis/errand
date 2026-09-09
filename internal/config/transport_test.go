package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDaemonTransportConfig(t *testing.T) {
	for _, tc := range []struct {
		body, mode, listen string
		bad                bool
	}{
		{"", "both", "tailnet:7443", false},
		{"listen = 'none'", "both", "none", false},
		{"transport = 'both'\nlisten = 'none'", "both", "none", false},
		{"transport = 'ssh'\nlisten = 'tailnet:7443'", "ssh", "none", false},
		{"transport = 'tailscale'\nlisten = 'none'", "tailscale", "tailnet:7443", false},
		{"transport = 'local'\nlisten = 'tailnet:7443'", "local", "none", false},
		{"transport = 'invalid'", "", "", true},
	} {
		p := filepath.Join(t.TempDir(), "runner.toml")
		if err := os.WriteFile(p, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadDaemon(p)
		if tc.bad {
			if err == nil {
				t.Fatal("accepted invalid transport")
			}
			continue
		}
		if err != nil || cfg.Transport != tc.mode || cfg.Listen != tc.listen {
			t.Fatalf("%q: %v / %+v", tc.body, err, cfg)
		}
	}
}
