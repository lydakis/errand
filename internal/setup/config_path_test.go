package setup

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSetupUsesSelectedConfigDirectory(t *testing.T) {
	for _, existing := range []bool{false, true} {
		f := newFake(t, "linux")
		f.env["XDG_CONFIG_HOME"] = "/custom/config"
		other := filepath.Join(f.home, ".config", "errand", "errandd.toml")
		f.files[other] = "transport = 'tailscale'\n"
		path := "/custom/config/errand/errandd.toml"
		if existing {
			f.files[path] = "transport = 'ssh'\nlisten = 'none'\n"
		}
		r, err := Run(context.Background(), Options{Transport: "ssh", DryRun: true}, f)
		if err != nil || r.Failed() || r.ConfigPath != path || r.Config.Transport != "ssh" {
			t.Fatalf("XDG config: %v / %+v", err, r)
		}
		r, err = Run(context.Background(), Options{ConfigPath: other, DryRun: true}, f)
		if err != nil || r.Failed() || r.ConfigPath != other || r.Config.Transport != "tailscale" {
			t.Fatalf("explicit config: %v / %+v", err, r)
		}
	}
}
