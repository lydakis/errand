package setup

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/lydakis/errand/internal/config"
)

func TestLocalSetupPreservesModeWithoutRemoteAccess(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			f := newFake(t, platform)
			f.probeInfo.LocalOnly, f.probeInfo.SSHDisabled = true, true
			r, err := Run(context.Background(), Options{Transport: "local"}, f)
			if err != nil || r.Failed() || r.Config.Transport != "local" || r.Config.Listen != "none" {
				t.Fatalf("local setup: %v / %+v", err, r)
			}
			original := f.files[r.ConfigPath]
			f.writes = nil
			r, err = Run(context.Background(), Options{}, f)
			if err != nil || r.Failed() || r.Config.Transport != "local" || f.files[r.ConfigPath] != original || len(f.writes) != 0 {
				t.Fatalf("rerun: %v / %+v", err, r)
			}
			if f.discoverCalls != 0 || len(f.writableChecks) != 0 {
				t.Fatal("local setup discovered Tailscale or installed an SSH path")
			}
		})
	}
}

func TestLocalSetupTransitionPreservesRemotePolicy(t *testing.T) {
	f := newFake(t, "linux")
	path := f.home + "/.config/errand/errandd.toml"
	original := "transport = 'both'\nlisten = 'tailnet:9443'\nsocket = '/tmp/custom.sock'\nmax_jobs = 3\nallow_users = []\ndeny_users = ['denied@example.com']\ncapability = 'example.com/errand'\n"
	f.files[path] = original
	// Both a preview and a busy runner leave the existing remote service intact.
	r, err := Run(context.Background(), Options{Transport: "local", DryRun: true}, f)
	if err != nil || r.Failed() || r.Config.Listen != "none" || f.files[path] != original || len(f.commands) != 0 || len(f.writes) != 0 {
		t.Fatalf("preview: %v / %+v", err, r)
	}
	f.probeInfo.RunningJobs = 1
	r, err = Run(context.Background(), Options{Transport: "local"}, f)
	if err != nil || !r.Failed() || f.files[path] != original || ranServiceMutation(f) || len(f.writes) != 0 {
		t.Fatalf("busy transition: %v / %+v", err, r)
	}
	f.probeInfo.RunningJobs = 0
	for _, mode := range []string{"local", "", "ssh", "local", "tailscale", "local", "both"} {

		f.probeInfo.LocalOnly = mode == "local" || mode == ""
		f.probeInfo.SSHDisabled = f.probeInfo.LocalOnly || mode == "tailscale"
		r, err = Run(context.Background(), Options{Transport: mode}, f)
		if err != nil || r.Failed() {
			t.Fatalf("transition %q: %v / %+v", mode, err, r)
		}
		want := mode
		if want == "" {
			want = "local"
		}
		if r.Config.Transport != want || ((want == "local" || want == "ssh") && r.Config.Listen != "none") {
			t.Fatalf("transition %q: %+v", mode, r.Config)
		}
		var saved config.Daemon
		if err := toml.Unmarshal([]byte(f.files[path]), &saved); err != nil {
			t.Fatal(err)
		}
		if saved.Listen != "tailnet:9443" || saved.Socket != "/tmp/custom.sock" || saved.MaxJobs != 3 || len(saved.AllowUsers) != 0 || len(saved.DenyUsers) != 1 || saved.Capability != "example.com/errand" {
			t.Fatalf("lost saved policy: %+v", saved)
		}
	}
}

func TestLocalSetupRejectsTailnetOptions(t *testing.T) {
	for _, opts := range []Options{{Transport: "local", CLI: "/tailscale"}, {Transport: "local", Socket: "/ts.sock"}, {Transport: "local", AllowUsers: []string{"owner@example.com"}}} {
		f := newFake(t, "linux")
		if _, err := Run(context.Background(), opts, f); err == nil {
			t.Fatal("accepted tailnet options")
		}
		if len(f.writes) != 0 || len(f.commands) != 0 || f.discoverCalls != 0 {
			t.Fatal("modified local-only setup")
		}
	}
}

func TestLocalSetupRejectsPreservedServiceBeforeChanges(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		for _, override := range []string{"other-config", "listen"} {
			for _, dryRun := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/dry=%v", platform, override, dryRun), func(t *testing.T) {
					f := newFake(t, platform)
					path := f.home + "/.config/errand/errandd.toml"
					original := "transport = 'ssh'\nlisten = 'none'\n"
					f.files[path] = original
					serviceConfig := path
					if override == "other-config" {
						serviceConfig = f.home + "/custom-runner.toml"
						f.files[serviceConfig] = original
					}
					exe, _ := f.Executable()
					unitPath := f.home + "/.config/systemd/user/errand.service"
					definition := renderSystemdUnit(exe, serviceConfig, servicePath(f.Getenv("PATH")))
					if platform == "darwin" {
						unitPath = f.home + "/Library/LaunchAgents/" + LaunchAgentLabel + ".plist"
						definition = renderLaunchAgent(LaunchAgentLabel, exe, serviceConfig, f.home+"/Library/Logs/errand/errand.log", servicePath(f.Getenv("PATH")))
					}
					if override == "listen" {
						if platform == "linux" {
							definition = strings.Replace(definition, " serve --config ", " serve --listen tailnet:7443 --config ", 1)
						} else {
							definition = strings.Replace(definition, "<string>serve</string>", "<string>serve</string><string>--listen</string><string>tailnet:7443</string>", 1)
						}
					}
					f.files[unitPath] = definition
					r, err := Run(context.Background(), Options{Transport: "local", DryRun: dryRun}, f)
					if err != nil || !r.Failed() {
						t.Fatalf("expected failed preflight: %v / %+v", err, r)
					}
					if f.files[path] != original || f.files[unitPath] != definition || len(f.writes) != 0 || len(f.commands) != 0 || len(f.quiesceSockets) != 0 {
						t.Fatal("changed state before rejecting incompatible service")
					}

					f.probeInfo.LocalOnly, f.probeInfo.SSHDisabled = true, true
					r, err = Run(context.Background(), Options{Transport: "local", Force: true, DryRun: dryRun}, f)
					if err != nil || r.Failed() {
						t.Fatalf("explicit service replacement: %v / %+v", err, r)
					}
					if !dryRun && f.files[unitPath] == definition {
						t.Fatal("force preserved incompatible service")
					}
				})
			}
		}
	}
}

func TestLocalSetupRequiresRunningModeConfirmation(t *testing.T) {
	f := newFake(t, "linux")
	// An old or differently configured daemon can answer without confirming local-only mode.
	r, err := Run(context.Background(), Options{Transport: "local"}, f)
	if err != nil || !r.Failed() {
		t.Fatalf("accepted unconfirmed local mode: %v / %+v", err, r)
	}
}

func TestLocalSetupUsesXDGConfigPath(t *testing.T) {
	f := newFake(t, "linux")
	f.env["XDG_CONFIG_HOME"] = "/custom/config"
	t.Setenv("HOME", f.home)
	t.Setenv("XDG_CONFIG_HOME", f.env["XDG_CONFIG_HOME"])
	path, err := config.DaemonPath()
	if err != nil {
		t.Fatal(err)
	}
	f.files[path] = "transport = 'local'\nsocket = '/custom/runner.sock'\n"
	r, err := Run(context.Background(), Options{Transport: "local", DryRun: true}, f)
	if err != nil || r.Failed() || r.ConfigPath != path || r.SocketPath != "/custom/runner.sock" {
		t.Fatalf("setup/resolver disagree: %v / %+v", err, r)
	}
	f.env["XDG_CONFIG_HOME"] = "relative"
	r, err = Run(context.Background(), Options{Transport: "local", DryRun: true}, f)
	if err == nil && !r.Failed() {
		t.Fatal("accepted relative XDG config directory")
	}
}
