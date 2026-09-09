package setup

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSSHSetupWithoutTailscale(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		for _, dryRun := range []bool{false, true} {
			name := goos
			if dryRun {
				name += "/dry-run"
			}
			t.Run(name, func(t *testing.T) {
				f := newFake(t, goos)
				f.discoverErr = errors.New("Tailscale is not installed")
				r, err := Run(context.Background(), Options{MaxJobs: 3, DryRun: dryRun}, f)
				if err != nil || r.Failed() {
					t.Fatalf("setup: %v / %+v", err, r)
				}
				if f.discoverCalls != 1 || !strings.Contains(stepDetail(r, "tailnet"), "Tailscale is not installed") || stepDetail(r, "ssh") == "" {
					t.Fatal("setup did not report automatic SSH-only selection")
				}
				if r.Config.Listen != "none" || r.Config.MaxJobs != 3 || r.Provider != "" {
					t.Fatalf("SSH config: %+v", r)
				}
				if dryRun {
					if len(f.writes) != 0 || len(f.commands) != 0 || len(f.symlinks) != 0 || len(f.probeSockets) != 0 || len(f.quiesceSockets) != 0 || len(f.writableChecks) != 0 {
						t.Fatalf("dry run touched the system: %+v", f)
					}
					return
				}
				cfg := f.files[r.ConfigPath]
				if !strings.Contains(cfg, `listen = "none"`) || strings.Contains(cfg, "allow_users") || strings.Contains(cfg, "tailscaled_socket") {
					t.Fatalf("SSH configuration: %s", cfg)
				}
				command := "systemctl --user restart errand.service"
				servicePath := f.home + "/.config/systemd/user/errand.service"
				if goos == "darwin" {
					command = "launchctl bootstrap gui/501"
					servicePath = f.home + "/Library/LaunchAgents/dev.lydakis.errand.plist"
				}
				if !ran(f, command) || !strings.Contains(f.files[servicePath], "serve") {
					t.Fatalf("service not installed and started: %v / %v", f.commands, f.files)
				}
				if goos == "linux" && !ran(f, "loginctl enable-linger george") {
					t.Fatal("missing linger")
				}
				if r.Info == nil || len(f.probeSockets) == 0 || f.probeSockets[len(f.probeSockets)-1] != r.SocketPath {
					t.Fatalf("runner not verified through its socket: %+v", r)
				}
			})
		}
	}
}

func TestSSHSetupHonorsSavedModeAndSettings(t *testing.T) {
	for _, force := range []bool{false, true} {
		f := newFake(t, "linux")
		f.discoverErr = errors.New("Tailscale is unavailable")
		path := f.home + "/.config/errand/errandd.toml"
		original := "transport = 'ssh'\nlisten = \" NONE \"\nsocket = \"/tmp/custom.sock\"\nmax_jobs = 3\nmax_queued = 5\n# operator settings\n"
		f.files[path] = original
		f.files[f.home+"/.config/systemd/user/errand.service"] = "# operator service\n"
		r, err := Run(context.Background(), Options{Force: force}, f)
		if err != nil || r.Failed() {
			t.Fatalf("force=%v: %v / %+v", force, err, r)
		}
		if f.discoverCalls != 0 || !strings.EqualFold(strings.TrimSpace(r.Config.Listen), "none") {
			t.Fatalf("force=%v changed mode: %+v", force, r)
		}
		if len(f.quiesceSockets) == 0 || f.quiesceSockets[0] != "/tmp/custom.sock" {
			t.Fatalf("did not inspect old socket: %v", f.quiesceSockets)
		}
		if !force && (f.files[path] != original || r.SocketPath != "/tmp/custom.sock" || r.Config.MaxJobs != 3 || f.files[f.home+"/.config/systemd/user/errand.service"] != "# operator service\n") {
			t.Fatal("rerun lost saved configuration")
		}
	}
}

func TestSSHSetupRejectsTailnetOptionsBeforeChanges(t *testing.T) {
	for _, opts := range []Options{{Socket: "/run/tailscale.sock"}, {CLI: "/bin/tailscale"}, {AllowUsers: []string{"other@example.com"}}} {
		f := newFake(t, "linux")
		f.files[f.home+"/.config/errand/errandd.toml"] = "transport = 'ssh'\nlisten = \"none\"\n"
		_, err := Run(context.Background(), opts, f)
		if err == nil || !strings.Contains(err.Error(), "SSH-only") {
			t.Fatalf("expected conflict: %v", err)
		}
		if len(f.writes) != 0 || len(f.commands) != 0 || f.discoverCalls != 0 {
			t.Fatal("conflict touched system")
		}
	}
}

func TestSSHSetupPreservesBusyRunner(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		f := newFake(t, goos)
		f.discoverErr = errors.New("Tailscale is unavailable")
		f.probeInfo.RunningJobs = 1
		f.files[f.home+"/.config/errand/errandd.toml"] = "transport = 'ssh'\nlisten = \"none\"\n"
		r, err := Run(context.Background(), Options{Force: true}, f)
		if err != nil || !r.Failed() || !strings.Contains(stepErrorDetail(r, "service"), "active jobs") {
			t.Fatalf("busy runner: %v / %+v", err, r)
		}
		if len(f.writes) != 0 || ranServiceMutation(f) || f.discoverCalls != 0 {
			t.Fatal("changed busy runner")
		}
	}
}

// An outage on an established runner must not rewrite its access policy.
func TestConfiguredTailnetSetupDoesNotFallBackToSSH(t *testing.T) {
	for _, force := range []bool{false, true} {
		for _, selfFailure := range []bool{false, true} {
			f := newFake(t, "linux")
			original := "listen = \"tailnet:7443\"\nallow_users = [\"owner@example.com\"]\n"
			configPath := f.home + "/.config/errand/errandd.toml"
			f.files[configPath] = original
			if selfFailure {
				p := f.provider.(fakeProvider)
				p.selfErr = errors.New("Tailscale is stopped")
				f.provider = p
			} else {
				f.discoverErr = errors.New("Tailscale is unavailable")
			}
			_, err := Run(context.Background(), Options{Force: force}, f)
			if err == nil {
				t.Fatalf("force=%v selfFailure=%v: expected failure", force, selfFailure)
			}
			if f.files[configPath] != original || len(f.writes) != 0 || len(f.commands) != 0 || len(f.quiesceSockets) != 0 {
				t.Fatal("tailnet outage changed the system")
			}
		}
	}
}

func TestFreshSetupUsesSSHWhenTailnetIdentityIsUnavailable(t *testing.T) {
	f := newFake(t, "linux")
	p := f.provider.(fakeProvider)
	p.selfErr = errors.New("Tailscale is stopped")
	f.provider = p
	r, err := Run(context.Background(), Options{}, f)
	if err != nil || r.Failed() || r.Config.Listen != "none" || r.Provider != "" || r.Self.Login != "" {
		t.Fatalf("automatic SSH setup: %v / %+v", err, r)
	}
	if !strings.Contains(stepDetail(r, "tailnet"), "Tailscale is stopped") || stepDetail(r, "ssh") == "" {
		t.Fatalf("missing mode selection explanation: %+v", r.Steps)
	}
}

func TestExplicitTailnetOptionsDoNotFallBackToSSH(t *testing.T) {
	for _, opts := range []Options{{Socket: "/run/tailscale.sock"}, {CLI: "/bin/tailscale"}, {AllowUsers: []string{"other@example.com"}}} {
		for _, selfFailure := range []bool{false, true} {
			f := newFake(t, "linux")
			if selfFailure {
				p := f.provider.(fakeProvider)
				p.selfErr = errors.New("Tailscale is stopped")
				f.provider = p
			} else {
				f.discoverErr = errors.New("Tailscale is unavailable")
			}
			_, err := Run(context.Background(), opts, f)
			if err == nil {
				t.Fatal("explicit tailnet request selected SSH-only access")
			}
			if len(f.writes) != 0 || len(f.commands) != 0 || len(f.quiesceSockets) != 0 {
				t.Fatal("changed system after discovery failure")
			}
		}
	}
}

func TestCanceledDiscoveryDoesNotInstallSSHRunner(t *testing.T) {
	f := newFake(t, "linux")
	f.discoverErr = context.Canceled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, Options{}, f)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
	if len(f.writes) != 0 || len(f.commands) != 0 {
		t.Fatal("cancellation installed a runner")
	}
}

func TestForcedInvalidConfigDoesNotFallBackToSSH(t *testing.T) {
	f := newFake(t, "linux")
	f.discoverErr = errors.New("Tailscale is unavailable")
	f.files[f.home+"/.config/errand/errandd.toml"] = "invalid TOML"
	_, err := Run(context.Background(), Options{Force: true}, f)
	if err == nil || len(f.writes) != 0 || len(f.commands) != 0 {
		t.Fatal("could not determine saved mode but rewrote it")
	}
}
