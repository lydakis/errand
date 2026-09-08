package setup

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupRefusesImplicitXDGTransition(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		for _, force := range []bool{false, true} {
			for _, dryRun := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/force=%t/dry=%t", platform, force, dryRun), func(t *testing.T) {
					f := newFake(t, platform)
					f.env["XDG_CONFIG_HOME"] = "/custom/config"
					legacy := filepath.Join(f.home, ".config", "errand", "errandd.toml")
					f.files[legacy] = "transport = 'ssh'\nlisten = 'none'\n"
					r, err := Run(context.Background(), Options{Force: force, DryRun: dryRun}, f)
					if err != nil || !r.Failed() {
						t.Fatalf("accepted implicit config transition: %v / %+v", err, r)
					}
					if !strings.Contains(fmt.Sprint(r.Steps), "--config") || !strings.Contains(fmt.Sprint(r.Steps), legacy) {
						t.Fatalf("missing recovery guidance: %+v", r.Steps)
					}
					if len(f.writes) != 0 || len(f.commands) != 0 || len(f.quiesceSockets) != 0 || f.discoverCalls != 0 {
						t.Fatalf("setup touched the host before resolving config ambiguity: %+v", f)
					}
					// Explicitly selecting the saved config retains its transport.
					r, err = Run(context.Background(), Options{ConfigPath: legacy, Force: force, DryRun: true}, f)
					if err != nil || r.Failed() || r.Config.Transport != "ssh" || r.ConfigPath != legacy {
						t.Fatalf("explicit legacy config: %v / %+v", err, r)
					}
				})
			}
		}
	}
}

func TestSetupUsesExistingXDGConfigAlongsideLegacy(t *testing.T) {
	f := newFake(t, "linux")
	f.env["XDG_CONFIG_HOME"] = "/custom/config"
	f.files[filepath.Join(f.home, ".config", "errand", "errandd.toml")] = "transport = 'tailscale'\n"
	path := "/custom/config/errand/errandd.toml"
	f.files[path] = "transport = 'ssh'\nlisten = 'none'\n"
	r, err := Run(context.Background(), Options{DryRun: true}, f)
	if err != nil || r.Failed() || r.ConfigPath != path || r.Config.Transport != "ssh" {
		t.Fatalf("existing XDG config: %v / %+v", err, r)
	}
}
