package setup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/tailnet"
)

type diagnosticSystem struct {
	*fakeSystem
	daemon     config.Daemon
	configErr  error
	mode       os.FileMode
	statErr    error
	configPath string
}

func (s *diagnosticSystem) LoadDaemon(path string) (config.Daemon, error) {
	s.configPath = path
	return s.daemon, s.configErr
}
func (s *diagnosticSystem) LookPath(string) (string, error) { return s.Executable() }
func (s *diagnosticSystem) DaemonPath() (string, error) {
	return filepath.Join(s.home, ".config", "errand", "errandd.toml"), nil
}
func (s *diagnosticSystem) Lstat(path string) (os.FileInfo, error) {
	if path == s.daemon.SocketPath() {
		return s.Stat(path)
	}
	if _, ok := s.files[path]; ok {
		return diagnosticFile{0o600}, nil
	}
	if err := s.readErr[path]; err != nil {
		return nil, err
	}
	return nil, os.ErrNotExist
}
func (s *diagnosticSystem) Stat(path string) (os.FileInfo, error) {
	if path == s.daemon.SocketPath() {
		return diagnosticFile{s.mode}, s.statErr
	}
	return diagnosticFile{0o755}, nil
}

type diagnosticFile struct{ mode os.FileMode }

func (f diagnosticFile) Name() string       { return "fixture" }
func (f diagnosticFile) Size() int64        { return 0 }
func (f diagnosticFile) Mode() os.FileMode  { return f.mode }
func (f diagnosticFile) ModTime() time.Time { return time.Time{} }
func (f diagnosticFile) IsDir() bool        { return f.mode.IsDir() }
func (f diagnosticFile) Sys() any           { return nil }

func healthyDiagnostics(t *testing.T, platform string) *diagnosticSystem {
	f := newFake(t, platform)
	f.probeInfo.Proto = proto.ProtoVersion
	f.cmdOutput["systemctl --user is-active errand.service"] = "active"
	f.cmdOutput["loginctl show-user george -p Linger"] = "Linger=yes"
	f.files[filepath.Join(f.home, ".config", "errand", "errandd.toml")] = ""
	return &diagnosticSystem{fakeSystem: f, daemon: config.Daemon{Listen: "none", StateDir: "/runner", Socket: "/custom/runner.sock"}, mode: os.ModeSocket | 0o600}
}

func TestDiagnoseDistinguishesUnconfiguredAndBrokenRunners(t *testing.T) {
	for _, state := range []string{"client only", "saved config", "installed service", "loaded service", "explicit config", "unreadable config"} {
		t.Run(state, func(t *testing.T) {
			s := healthyDiagnostics(t, "linux")
			s.files = map[string]string{}
			s.cmdOutput["systemctl --user is-active errand.service"] = "inactive"
			s.statErr = os.ErrNotExist
			s.provider = nil // client-only diagnosis must not require Tailscale
			cfg := ""
			switch state {
			case "saved config":
				path, _ := s.DaemonPath()
				s.files[path] = ""
			case "installed service":
				s.files[filepath.Join(s.home, linuxUnitSubdir, "errand.service")] = ""
			case "loaded service":
				s.cmdOutput["systemctl --user is-active errand.service"] = "active"
			case "explicit config":
				cfg = "/custom/runner.toml"
			case "unreadable config":
				path, _ := s.DaemonPath()
				s.readErr[path] = os.ErrPermission
				s.configErr = os.ErrPermission
			}
			r := Diagnose(context.Background(), cfg, s)
			if r.OK() != (state == "client only") || r.Info != nil || len(s.probeSockets) != 0 {
				t.Fatalf("%+v", r)
			}
			if state == "client only" {
				if r.SocketPath != "" {
					t.Fatal("unconfigured socket reported as a target")
				}
				for _, c := range r.Checks {
					if c.Name == "ssh-path" || c.Name == "tailnet" || c.Status == "error" {
						t.Fatalf("client-only machine got runner checks: %+v", r)
					}
				}
			}
		})
	}
}

func TestDiagnoseStoppedInstalledServiceFailsEvenWithResponsiveSocket(t *testing.T) {
	s := healthyDiagnostics(t, "linux")
	s.files[filepath.Join(s.home, linuxUnitSubdir, "errand.service")] = ""
	s.cmdOutput["systemctl --user is-active errand.service"] = "inactive"
	r := Diagnose(context.Background(), "", s)
	if r.OK() || r.Info == nil {
		t.Fatalf("%+v", r)
	}
}

func TestDiagnoseUsesRunnerConfigAndNeverMutates(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		s := healthyDiagnostics(t, platform)
		r := Diagnose(context.Background(), "/custom/runner.toml", s)
		if !r.OK() || r.Info == nil || s.configPath != "/custom/runner.toml" || len(s.probeSockets) != 1 || s.probeSockets[0] != "/custom/runner.sock" {
			t.Fatalf("%s: %+v", platform, r)
		}
		if len(s.writes)+len(s.writableChecks)+len(s.quiesceSockets)+len(s.releasedLeases) != 0 || len(s.symlinks) != 0 {
			t.Fatal("diagnosis mutated the runner")
		}
		for _, command := range s.commands {
			if command != "systemctl --user is-active errand.service" && command != "loginctl show-user george -p Linger" && command != "launchctl print gui/501/dev.lydakis.errand" {
				t.Fatalf("unexpected service command %s", command)
			}
		}
		if s.discoverSocket != "" || s.discoverCLI != "" {
			t.Fatal("SSH-only runner required tailnet discovery")
		}
	}
}

func TestDiagnoseReportsIndependentFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*diagnosticSystem)
		want   string
		probes int
	}{
		{"config", func(s *diagnosticSystem) { s.configErr = errors.New("invalid config") }, "configuration", 0},
		{"missing socket", func(s *diagnosticSystem) { s.statErr = os.ErrNotExist }, "socket", 0},
		{"not a socket", func(s *diagnosticSystem) { s.mode = 0o600 }, "socket", 0},
		{"socket permissions", func(s *diagnosticSystem) { s.mode = os.ModeSocket | 0o666 }, "socket", 1},
		{"stale socket", func(s *diagnosticSystem) { s.probeErr = errors.New("connection refused") }, "runner", 1},
		{"protocol", func(s *diagnosticSystem) { s.probeInfo.Proto = proto.ProtoVersion + 1 }, "runner", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := healthyDiagnostics(t, "linux")
			tc.change(s)
			r := Diagnose(context.Background(), "", s)
			found := false
			for _, c := range r.Checks {
				if c.Name == tc.want && c.Status == "error" {
					found = true
				}
			}
			if r.OK() || !found || len(s.probeSockets) != tc.probes {
				t.Fatalf("unexpected report: %+v", r)
			}
			if len(s.commands) == 0 {
				t.Fatal("independent service checks were skipped")
			}
		})
	}
}

func TestDiagnoseDoesNotPrintServiceEnvironment(t *testing.T) {
	s := healthyDiagnostics(t, "darwin")
	s.cmdErr["launchctl print gui/501/dev.lydakis.errand"] = errors.New("failed: PRIVATE_TEST_VALUE")
	r := Diagnose(context.Background(), "", s)
	for _, c := range r.Checks {
		if strings.Contains(c.Detail+c.Hint, "PRIVATE_TEST_VALUE") {
			t.Fatal("service output leaked")
		}
	}
	if !r.OK() {
		t.Fatal("a manually managed healthy runner should only warn about the setup service")
	}
}

func TestDiagnoseTailnetFailureStillProbesLocalRunner(t *testing.T) {
	s := healthyDiagnostics(t, "linux")
	s.daemon.Listen = "tailnet:7443"
	s.daemon.TailscaleCLI = "/custom/tailscale"
	s.provider = fakeProvider{name: "cli:/custom/tailscale", self: tailnet.Self{Version: "1.90.0"}}
	r := Diagnose(context.Background(), "", s)
	if r.OK() || r.Info == nil || s.discoverCLI != "/custom/tailscale" {
		t.Fatalf("tailnet failure lost independent runner check: %+v", r)
	}
}

func TestDiagnoseRequiresRunningTailnetDespiteCachedIdentity(t *testing.T) {
	for _, state := range []string{"Running", "Stopped"} {
		t.Run(state, func(t *testing.T) {
			s := healthyDiagnostics(t, "linux")
			s.daemon.Listen = "tailnet:7443"
			cli := filepath.Join(t.TempDir(), "tailscale")
			body := fmt.Sprintf("#!/bin/sh\ncat <<'JSON'\n"+
				`{"Version":"1.102.3","BackendState":%q,"Self":{"UserID":42,"TailscaleIPs":["100.64.0.9"]},"User":{"42":{"LoginName":"test@example.invalid"}}}`+"\nJSON\n", state)
			if err := os.WriteFile(cli, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			s.provider = tailnet.NewCLI(cli)
			r := Diagnose(context.Background(), "", s)
			if r.OK() != (state == "Running") || r.Info == nil {
				t.Fatalf("backend %s: %+v", state, r)
			}
			if state == "Stopped" {
				found := false
				for _, check := range r.Checks {
					if check.Name == "tailnet" && check.Status == "error" && strings.Contains(check.Detail, state) {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing stopped-backend diagnostic: %+v", r)
				}
			}
		})
	}
}

func TestLocalProbeValidatesCompleteInfoDocument(t *testing.T) {
	for _, body := range []string{`{"version":"test","proto":0}`, `{"version":"test"}`, `{"version":"test","proto":1}`, `{"version":"test","proto":0} trailing`, strings.Repeat(" ", (1<<20)+1)} {
		// Keep the Unix path below macOS's socket-path limit.
		dir, err := os.MkdirTemp("/tmp", "errand-diagnose-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		socket := filepath.Join(dir, "s")
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || r.URL.Path != "/v0/info" {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
			_, _ = w.Write([]byte(body))
		})}
		go func() { _ = server.Serve(listener) }()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err = (RealSystem{}).Probe(ctx, socket)
		cancel()
		_ = server.Close()
		wantOK := body == `{"version":"test","proto":0}`
		if (err == nil) != wantOK {
			t.Fatalf("valid=%v error=%v", wantOK, err)
		}
	}
}
