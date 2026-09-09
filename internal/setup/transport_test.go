package setup

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/lydakis/errand/internal/config"
)

func TestSetupTransportPreferences(t *testing.T) {
	for _, mode := range []string{"both", "ssh", "tailscale", "local"} {
		for _, goos := range []string{"linux", "darwin"} {
			t.Run(mode+"/"+goos, func(t *testing.T) {
				f := newFake(t, goos)
				f.probeInfo.LocalOnly = mode == "local"
				f.probeInfo.SSHDisabled = mode == "local" || mode == "tailscale"
				r, err := Run(context.Background(), Options{Transport: mode}, f)
				if err != nil || r.Failed() || r.Config.Transport != mode {
					t.Fatalf("setup: %v / %+v", err, r)
				}
				if mode == "ssh" || mode == "local" {
					if f.discoverCalls != 0 || r.Config.Listen != "none" {
						t.Fatal("socket-only enabled Tailscale")
					}
				} else if f.discoverCalls != 1 || r.Config.Listen != "tailnet:7443" {
					t.Fatal("tailnet not configured")
				}
				if (mode == "tailscale" || mode == "local") && len(f.writableChecks) != 0 {
					t.Fatal("mode with SSH disabled installed SSH path")
				}
				var cfg config.Daemon
				if err := toml.Unmarshal([]byte(f.files[r.ConfigPath]), &cfg); err != nil {
					t.Fatal(err)
				}
				if cfg.Transport != mode {
					t.Fatalf("saved config: %+v", cfg)
				}
			})
		}
	}
}

func TestSetupBothPicksUpTailscaleAndPreservesSettings(t *testing.T) {
	f := newFake(t, "linux")
	f.discoverErr = errors.New("Tailscale is not installed")
	r, err := Run(context.Background(), Options{MaxJobs: 3}, f)
	if err != nil || r.Failed() || r.Config.Transport != "both" || r.Config.Listen != "none" {
		t.Fatalf("initial setup: %v / %+v", err, r)
	}
	path := r.ConfigPath
	f.files[path] += "max_queued = 6\nsocket = \"/tmp/custom.sock\"\ndeny_users = [\"denied@example.com\"]\n[cache]\nmax_bytes = 12345\n"
	original := f.files[path]
	f.writes = nil
	r, err = Run(context.Background(), Options{}, f)
	if err != nil || r.Failed() || f.files[path] != original || len(f.writes) != 0 {
		t.Fatalf("pending rerun changed settings: %v / %+v", err, r)
	}
	f.discoverErr = nil
	f.files[f.home+"/.config/systemd/user/errand.service"] += "# user customization\n"
	service := f.files[f.home+"/.config/systemd/user/errand.service"]
	r, err = Run(context.Background(), Options{}, f)
	if err != nil || r.Failed() || r.Config.Transport != "both" || r.Config.Listen != "tailnet:7443" {
		t.Fatalf("upgrade: %v / %+v", err, r)
	}
	var cfg config.Daemon
	if err := toml.Unmarshal([]byte(f.files[path]), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.MaxJobs != 3 || cfg.MaxQueued != 6 || cfg.Socket != "/tmp/custom.sock" || cfg.Cache.MaxBytes != 12345 || len(cfg.DenyUsers) != 1 || cfg.DenyUsers[0] != "denied@example.com" {
		t.Fatalf("lost settings: %+v", cfg)
	}
	if len(cfg.AllowUsers) != 1 || cfg.AllowUsers[0] != "george@example.com" {
		t.Fatalf("missing initial tailnet owner: %+v", cfg)
	}
	if f.files[f.home+"/.config/systemd/user/errand.service"] != service {
		t.Fatal("overwrote custom service")
	}
	original = f.files[path]
	f.writes = nil
	_, err = Run(context.Background(), Options{}, f)
	if err != nil || len(f.writes) != 0 || f.files[path] != original {
		t.Fatal("unchanged rerun rewrote config")
	}
	f.discoverErr = errors.New("temporary outage")
	f.commands = nil
	_, err = Run(context.Background(), Options{}, f)
	if err == nil || len(f.writes) != 0 || len(f.commands) != 0 || f.files[path] != original {
		t.Fatal("outage changed an enabled path")
	}
}

func TestSetupModeChangesPreserveExplicitPolicy(t *testing.T) {
	f := newFake(t, "linux")
	path := f.home + "/.config/errand/errandd.toml"
	f.files[path] = "transport = 'both'\nlisten = 'tailnet:9443'\nmax_jobs = 4\nallow_users = []\ndeny_users = ['george@example.com']\ncapability = 'custom.example/cap'\n"
	r, err := Run(context.Background(), Options{Transport: "ssh"}, f)
	if err != nil || r.Failed() || r.Config.Transport != "ssh" || r.Config.Listen != "none" {
		t.Fatalf("switch to SSH: %v / %+v", err, r)
	}
	if f.discoverCalls != 0 {
		t.Fatal("SSH choice required Tailscale")
	}
	r, err = Run(context.Background(), Options{Transport: "tailscale"}, f)
	if err != nil || r.Failed() || r.Config.Transport != "tailscale" {
		t.Fatalf("switch to tailnet: %v / %+v", err, r)
	}
	var cfg config.Daemon
	if err := toml.Unmarshal([]byte(f.files[path]), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "tailnet:9443" || cfg.MaxJobs != 4 || len(cfg.AllowUsers) != 0 || len(cfg.DenyUsers) != 1 || cfg.Capability != "custom.example/cap" {
		t.Fatalf("lost policy: %+v", cfg)
	}
	// Editing the config is equivalent to choosing a mode at the CLI.
	f.files[path] = strings.Replace(f.files[path], `transport = "tailscale"`, `transport = "ssh"`, 1)
	calls := f.discoverCalls
	r, err = Run(context.Background(), Options{}, f)
	if err != nil || r.Failed() || r.Config.Transport != "ssh" || r.Config.Listen != "none" || f.discoverCalls != calls {
		t.Fatalf("config edit ignored: %v / %+v", err, r)
	}
}

func TestSetupTransportUpgradePreviewAndBusyRunner(t *testing.T) {
	f := newFake(t, "linux")
	path := f.home + "/.config/errand/errandd.toml"
	original := "transport = 'both'\nlisten = 'none'\n"
	f.files[path] = original
	r, err := Run(context.Background(), Options{DryRun: true}, f)
	if err != nil || r.Failed() || r.Config.Listen != "tailnet:7443" || !strings.Contains(stepDetail(r, "config"), "would write") {
		t.Fatalf("preview: %v / %+v", err, r)
	}
	if f.files[path] != original || len(f.writes) != 0 || len(f.commands) != 0 || len(f.quiesceSockets) != 0 {
		t.Fatal("preview changed system")
	}
	f.probeInfo.RunningJobs = 1
	r, err = Run(context.Background(), Options{}, f)
	if err != nil || !r.Failed() || f.files[path] != original || len(f.writes) != 0 || ranServiceMutation(f) {
		t.Fatal("upgraded busy runner")
	}
}

func TestSetupTailscaleOnlyRequiresTailscale(t *testing.T) {
	f := newFake(t, "linux")
	f.discoverErr = errors.New("Tailscale unavailable")
	_, err := Run(context.Background(), Options{Transport: "tailscale"}, f)
	if err == nil || len(f.writes) != 0 || len(f.commands) != 0 {
		t.Fatal("Tailscale-only fell back to SSH")
	}
}

// Omitted policy keys on an established runner are a default capability
// policy, not an invitation for setup to initialize an owner allowlist.
func TestSetupTransportChangesPreserveImplicitCapabilityPolicy(t *testing.T) {
	configs := []struct{ name, body string }{
		{"default transport and listener", ""},
		{"default transport with explicit listener", "listen = 'tailnet:9443'\n"},
		{"both default listener", "transport = 'both'\n"},
		{"both explicit listener", "transport = 'both'\nlisten = 'tailnet:9443'\n"},
		{"tailscale normalized listener", "transport = 'tailscale'\nlisten = ' NONE '\n"},
	}
	for _, cfg := range configs {
		for _, throughSSH := range []bool{false, true} {
			name := cfg.name + "/direct"
			if throughSSH {
				name = cfg.name + "/ssh round trip"
			}
			t.Run(name, func(t *testing.T) {
				f := newFake(t, "linux")
				path := f.home + "/.config/errand/errandd.toml"
				f.files[path] = cfg.body + "max_jobs = 3\ndeny_users = ['denied@example.com']\n"
				if throughSSH {
					r, err := Run(context.Background(), Options{Transport: "ssh"}, f)
					if err != nil || r.Failed() {
						t.Fatalf("SSH step: %v / %+v", err, r)
					}
				}
				for _, mode := range []string{"tailscale", "both"} {
					before := f.files[path]
					f.writes, f.commands = nil, nil
					preview, err := Run(context.Background(), Options{Transport: mode, DryRun: true}, f)
					if err != nil || preview.Failed() || len(preview.Config.AllowUsers) != 0 {
						t.Fatalf("preview changed implicit policy: %v / %+v", err, preview)
					}
					if f.files[path] != before || len(f.writes) != 0 || len(f.commands) != 0 {
						t.Fatal("preview mutated system")
					}
					r, err := Run(context.Background(), Options{Transport: mode}, f)
					if err != nil || r.Failed() || len(r.Config.AllowUsers) != 0 {
						t.Fatalf("mode %s changed implicit policy: %v / %+v", mode, err, r)
					}
					var document map[string]any
					if err := toml.Unmarshal([]byte(f.files[path]), &document); err != nil {
						t.Fatal(err)
					}
					if _, present := document["allow_users"]; present {
						t.Fatal("setup initialized an established runner's allowlist")
					}
					if _, present := document["capability"]; present {
						t.Fatal("setup rewrote implicit capability policy")
					}
					if r.Config.MaxJobs != 3 || len(r.Config.DenyUsers) != 1 || r.Config.DenyUsers[0] != "denied@example.com" {
						t.Fatalf("lost settings: %+v", r.Config)
					}
				}
			})
		}
	}
}

func TestSetupFirstTailnetActivationHonorsPolicyPresence(t *testing.T) {
	policies := []struct {
		name, body string
		seedOwner  bool
	}{
		{"no policy", "", true},
		{"denylist only", "deny_users = ['denied@example.com']\n", true},
		{"empty allowlist", "allow_users = []\n", false},
		{"implicit default made explicit", "capability = ''\n", false},
		{"custom capability", "capability = 'custom.example/cap'\n", false},
	}
	for _, mode := range []string{"both", "ssh"} {
		for _, policy := range policies {
			t.Run(mode+"/"+policy.name, func(t *testing.T) {
				f := newFake(t, "linux")
				path := f.home + "/.config/errand/errandd.toml"
				f.files[path] = "transport = '" + mode + "'\nlisten = ' NONE '\n" + policy.body
				r, err := Run(context.Background(), Options{Transport: "tailscale"}, f)
				if err != nil || r.Failed() {
					t.Fatalf("activation: %v / %+v", err, r)
				}
				if policy.seedOwner {
					if len(r.Config.AllowUsers) != 1 || r.Config.AllowUsers[0] != "george@example.com" {
						t.Fatalf("owner not initialized: %+v", r.Config)
					}
				} else if len(r.Config.AllowUsers) != 0 {
					t.Fatalf("existing policy broadened: %+v", r.Config)
				}
				var before, after map[string]any
				if err := toml.Unmarshal([]byte(policy.body), &before); err != nil {
					t.Fatal(err)
				}
				if err := toml.Unmarshal([]byte(f.files[path]), &after); err != nil {
					t.Fatal(err)
				}
				for key, value := range before {
					if !reflect.DeepEqual(value, after[key]) {
						t.Fatalf("policy key %s changed: %v -> %v", key, value, after[key])
					}
				}
			})
		}
	}
}

func TestSetupUnavailableTailnetPreservesEstablishedImplicitPolicy(t *testing.T) {
	for _, body := range []string{
		"transport = 'tailscale'\nlisten = 'none'\n",
		"transport = 'ssh'\nlisten = 'tailnet:9443'\n",
		"transport = 'ssh'\n",
	} {
		f := newFake(t, "linux")
		path := f.home + "/.config/errand/errandd.toml"
		f.files[path] = body
		f.discoverErr = errors.New("Tailscale unavailable")
		_, err := Run(context.Background(), Options{Transport: "both"}, f)
		if err == nil || f.files[path] != body || len(f.writes) != 0 || len(f.commands) != 0 {
			t.Fatalf("outage discarded configured policy: %v / %s", err, f.files[path])
		}
		f.discoverErr = nil
		r, err := Run(context.Background(), Options{Transport: "both"}, f)
		if err != nil || r.Failed() || len(r.Config.AllowUsers) != 0 {
			t.Fatalf("restored transport broadened policy: %v / %+v", err, r)
		}
	}
}

func TestSetupExplicitTransportRepairsInvalidSavedMode(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		for _, mode := range []string{"ssh", "tailscale", "both"} {
			for _, force := range []bool{false, true} {
				for _, listen := range []string{"none", "tailnet:9443"} {
					t.Run(fmt.Sprintf("%s/%s/force=%v/%s", goos, mode, force, listen), func(t *testing.T) {
						f := newFake(t, goos)
						path := f.home + "/.config/errand/errandd.toml"
						f.files[path] = fmt.Sprintf("transport = 'typo'\nlisten = '%s'\nmax_jobs = 4\nmax_queued = 6\nsocket = '/tmp/existing-errand.sock'\n", listen)
						r, err := Run(context.Background(), Options{Transport: mode, Force: force}, f)
						if err != nil || r.Failed() || r.Config.Transport != mode {
							t.Fatalf("repair: %v / %+v", err, r)
						}
						if len(f.quiesceSockets) != 1 || f.quiesceSockets[0] != "/tmp/existing-errand.sock" {
							t.Fatalf("wrong restart socket: %v", f.quiesceSockets)
						}
						var cfg config.Daemon
						if err := toml.Unmarshal([]byte(f.files[path]), &cfg); err != nil {
							t.Fatal(err)
						}
						if cfg.Transport != mode {
							t.Fatalf("saved transport: %q", cfg.Transport)
						}
						if !force {
							if cfg.MaxJobs != 4 || cfg.MaxQueued != 6 || cfg.Socket != "/tmp/existing-errand.sock" {
								t.Fatalf("lost settings: %+v", cfg)
							}
							var document map[string]any
							if err := toml.Unmarshal([]byte(f.files[path]), &document); err != nil {
								t.Fatal(err)
							}
							if _, ok := document["allow_users"]; ok {
								t.Fatal("repair replaced implicit capability policy")
							}
							if listen != "none" && cfg.Listen != listen {
								t.Fatalf("lost custom listener: %q", cfg.Listen)
							}
						} else if cfg.MaxJobs != 1 {
							t.Fatalf("force did not regenerate config: %+v", cfg)
						}
						if mode == "ssh" && f.discoverCalls != 0 {
							t.Fatal("SSH repair required Tailscale")
						}
					})
				}
			}
		}
	}
}

func TestSetupTransportValidationPrecedence(t *testing.T) {
	for _, force := range []bool{false, true} {
		for _, requested := range []string{"", "bad-request"} {
			t.Run(fmt.Sprintf("force=%v/request=%s", force, requested), func(t *testing.T) {
				f := newFake(t, "linux")
				path := f.home + "/.config/errand/errandd.toml"
				original := "transport = 'bad-saved'\n"
				f.files[path] = original
				_, err := Run(context.Background(), Options{Transport: requested, Force: force}, f)
				want := requested
				if want == "" {
					want = "bad-saved"
				}
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("expected error for %q, got %v", want, err)
				}
				if len(f.writes) != 0 || len(f.quiesceSockets) != 0 || f.discoverCalls != 0 || f.files[path] != original {
					t.Fatal("invalid transport caused side effects")
				}
			})
		}
	}
}

func TestSetupTransportRepairStillProtectsActiveRunner(t *testing.T) {
	for _, force := range []bool{false, true} {
		for _, socketSetting := range []string{"socket = '/tmp/existing/errand.sock'", "state_dir = '/tmp/existing'"} {
			t.Run(fmt.Sprintf("force=%v/%s", force, socketSetting), func(t *testing.T) {
				f := newFake(t, "linux")
				path := f.home + "/.config/errand/errandd.toml"
				original := "transport = 'typo'\n" + socketSetting + "\n"
				f.files[path] = original
				f.probeInfo.RunningJobs = 1
				r, err := Run(context.Background(), Options{Transport: "ssh", Force: force}, f)
				if err != nil || !r.Failed() {
					t.Fatalf("expected busy runner refusal: %v / %+v", err, r)
				}
				if len(f.quiesceSockets) != 1 || f.quiesceSockets[0] != "/tmp/existing/errand.sock" {
					t.Fatalf("wrong restart socket: %v", f.quiesceSockets)
				}
				if len(f.writes) != 0 || f.files[path] != original {
					t.Fatal("busy runner repair wrote config")
				}
			})
		}
	}
}
