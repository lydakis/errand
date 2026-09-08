package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/setup"
	"github.com/lydakis/errand/internal/tailnet"
)

type sshSetupDryRunSystem struct {
	setup.RealSystem
	home string
}

func (s sshSetupDryRunSystem) Home() (string, error) { return s.home, nil }
func (sshSetupDryRunSystem) Discover(string, string) (tailnet.Provider, error) {
	return nil, errors.New("Tailscale is not installed")
}
func (sshSetupDryRunSystem) Run(context.Context, string, ...string) (string, error) {
	panic("dry run must not run service commands")
}

func TestSetupSSHDryRunCLI(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sys := sshSetupDryRunSystem{home: t.TempDir()}
	code := cmdSetupTo([]string{"-n"}, &stdout, &stderr, sys)
	if code != 0 {
		t.Fatalf("exit %d: %s / %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{`listen = "none"`, "would be configured", `ssh = "YOUR_SSH_HOST"`, "remote_socket ="} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("missing %q: %s", want, stdout.String())
		}
	}
	for _, unwanted := range []string{"is ready", "url =", "allows:", "denies:"} {
		if strings.Contains(stdout.String(), unwanted) {
			t.Errorf("unexpected %q: %s", unwanted, stdout.String())
		}
	}
}

func TestSetupSSHReportWithoutTailnetIdentity(t *testing.T) {
	r := &setup.Report{
		Config:        setup.ConfigChoice{Listen: "none", AllowUsers: []string{"ignored@example.com"}, DenyUsers: []string{"ignored@example.com"}},
		RemoteCommand: "/opt/errand", SocketPath: "/srv/errand.sock",
	}
	var output bytes.Buffer
	printSetupReport(&output, r, false)
	for _, want := range []string{"is ready", `ssh = "YOUR_SSH_HOST"`, `remote_command = "/opt/errand"`, `remote_socket = "/srv/errand.sock"`} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing %q: %s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "ignored@example.com") || strings.Contains(output.String(), "url =") {
		t.Fatal(output.String())
	}
}

func TestSetupTransportFlagConflicts(t *testing.T) {
	for _, args := range [][]string{{"--local", "--ssh"}, {"--local", "--tailscale"}, {"--local", "--print-acl"}, {"--local", "--allow-user", "friend@example.com"}, {"--ssh", "--tailscale"}, {"--ssh", "--allow-user", "friend@example.com"}, {"--ssh", "--print-acl"}} {
		var out, errOut bytes.Buffer
		if code := cmdSetupTo(args, &out, &errOut, nil); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}

func TestSetupSSHFlagOverridesAutomaticDetection(t *testing.T) {
	var out, errOut bytes.Buffer
	sys := sshSetupDryRunSystem{home: t.TempDir()}
	if code := cmdSetupTo([]string{"--ssh", "-n"}, &out, &errOut, sys); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `transport = "ssh"`) || strings.Contains(out.String(), "Tailscale is not installed") {
		t.Fatal(out.String())
	}
}

func TestSetupTailscaleReportOmitsSSHInstructions(t *testing.T) {
	r := &setup.Report{Config: setup.ConfigChoice{Transport: "tailscale", Listen: "tailnet:7443"}, Self: tailnet.Self{DNSName: "runner.example.ts.net"}}
	var out bytes.Buffer
	printSetupReport(&out, r, false)
	if !strings.Contains(out.String(), "url =") || strings.Contains(out.String(), "ssh =") || strings.Contains(out.String(), "Or use SSH") {
		t.Fatal(out.String())
	}
}

func TestSetupTailscaleFlagDoesNotUseAutomaticSSHFallback(t *testing.T) {
	var out, errOut bytes.Buffer
	sys := sshSetupDryRunSystem{home: t.TempDir()}
	if code := cmdSetupTo([]string{"--tailscale", "-n"}, &out, &errOut, sys); code != 1 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "Tailscale is not installed") || strings.Contains(out.String(), "would be configured") {
		t.Fatalf("%s / %s", out.String(), errOut.String())
	}
}

type transportSetupDryRunSystem struct{ sshSetupDryRunSystem }
type setupTestProvider struct{ serveTestProvider }

func (setupTestProvider) Self(context.Context) (tailnet.Self, error) {
	return tailnet.Self{Version: "1.102.3", Login: "owner@example.com", DNSName: "runner.example.ts.net"}, nil
}
func (transportSetupDryRunSystem) Discover(string, string) (tailnet.Provider, error) {
	return setupTestProvider{}, nil
}

func TestSetupTransportRepairFlagSpellings(t *testing.T) {
	for _, mode := range []string{"ssh", "tailscale", "local"} {
		for _, force := range []bool{false, true} {
			name := mode + "/preserve"
			if force {
				name = mode + "/force"
			}
			t.Run(name, func(t *testing.T) {
				home := t.TempDir()
				path := filepath.Join(home, "errandd.toml")
				original := "transport = 'typo'\nmax_jobs = 4\n"
				if err := os.WriteFile(path, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
				sys := transportSetupDryRunSystem{sshSetupDryRunSystem{home: home}}
				var expected string
				for i, args := range [][]string{
					{"--" + mode, "--dry-run", "--config", path},
					{"--" + mode, "-n", "--config", path},
					{"-" + mode, "-n", "-config", path},
				} {
					if force {
						if i == 0 {
							args = append(args, "--force")
						} else {
							args = append([]string{"-f"}, args...)
						}
					}
					var out, errOut bytes.Buffer
					if code := cmdSetupTo(args, &out, &errOut, sys); code != 0 {
						t.Fatalf("%v: exit %d: %s / %s", args, code, out.String(), errOut.String())
					}
					if i == 0 {
						expected = out.String()
					} else if out.String() != expected {
						t.Fatalf("shorthand output differs: %v\n%s\nwant:\n%s", args, out.String(), expected)
					}
					wantJobs := "max_jobs = 4"
					if force {
						wantJobs = "max_jobs = 1"
					}
					if !strings.Contains(out.String(), `transport = "`+mode+`"`) || !strings.Contains(out.String(), wantJobs) {
						t.Fatalf("wrong repair: %s", out.String())
					}
					data, err := os.ReadFile(path)
					if err != nil || string(data) != original {
						t.Fatalf("dry run changed saved config: %v / %s", err, data)
					}
				}
			})
		}
	}
}
