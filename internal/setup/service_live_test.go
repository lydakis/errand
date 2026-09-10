//go:build darwin || linux

package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// liveServiceSystem changes only the service namespace and test paths. All
// service-manager commands, socket credentials, HTTP probes, and daemon
// processes are real. Never point these tests at the production service label.
type liveServiceSystem struct {
	RealSystem
	home, executable, label, unitDir string
	writes                           int
}

func (s *liveServiceSystem) Home() (string, error)       { return s.home, nil }
func (s *liveServiceSystem) Executable() (string, error) { return s.executable, nil }
func (s *liveServiceSystem) Getenv(key string) string {
	if key == "XDG_CONFIG_HOME" {
		return filepath.Join(s.home, ".config")
	}
	return os.Getenv(key)
}
func (s *liveServiceSystem) mappedPath(p string) string {
	if runtime.GOOS == "linux" && p == filepath.Join(s.home, linuxUnitSubdir, "errand.service") {
		return filepath.Join(s.unitDir, s.label+".service")
	}
	return p
}
func (s *liveServiceSystem) Exists(p string) bool { return s.RealSystem.Exists(s.mappedPath(p)) }
func (s *liveServiceSystem) ReadFile(p string) ([]byte, error) {
	b, err := s.RealSystem.ReadFile(s.mappedPath(p))
	if strings.HasSuffix(p, LaunchAgentLabel+".plist") {
		b = bytes.ReplaceAll(b, []byte(s.label), []byte(LaunchAgentLabel))
	}
	return b, err
}
func (s *liveServiceSystem) WriteFile(p string, b []byte, mode os.FileMode) error {
	s.writes++
	if strings.HasSuffix(p, LaunchAgentLabel+".plist") {
		b = bytes.ReplaceAll(b, []byte(LaunchAgentLabel), []byte(s.label))
	}
	return s.RealSystem.WriteFile(s.mappedPath(p), b, mode)
}
func (s *liveServiceSystem) Run(ctx context.Context, command string, args ...string) (string, error) {
	mapped := append([]string(nil), args...)
	for i, arg := range mapped {
		if command == "launchctl" && strings.HasSuffix(arg, "/"+LaunchAgentLabel) {
			mapped[i] = strings.TrimSuffix(arg, LaunchAgentLabel) + s.label
		}
		if command == "systemctl" && arg == "errand.service" {
			mapped[i] = s.label + ".service"
		}
	}
	return s.RealSystem.Run(ctx, command, mapped...)
}

func TestLiveServiceLifecycle(t *testing.T) {
	if os.Getenv("ERRAND_TEST_SERVICE_MANAGER") != "1" {
		t.Skip("opt in with ERRAND_TEST_SERVICE_MANAGER=1; creates and removes an isolated user service")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	real := RealSystem{}
	if runtime.GOOS == "darwin" {
		if out, err := real.Run(ctx, "launchctl", "print", "gui/"+uidString(real.UID())); err != nil {
			t.Fatalf("GUI launchd domain required: %v: %s", err, out)
		}
	} else {
		if _, err := real.Run(ctx, "systemctl", "--user", "show-environment"); err != nil {
			t.Fatalf("user systemd bus required: %v", err)
		}
		out, err := real.Run(ctx, "loginctl", "show-user", real.Username(), "-p", "Linger")
		if err != nil || strings.TrimSpace(out) != "Linger=yes" {
			t.Fatalf("pre-enable linger for the test account; this test does not change it: %v / %s", err, out)
		}
	}
	// Keep socket paths below macOS's sockaddr_un limit, even in CI.
	home, err := os.MkdirTemp("/tmp", "errand-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(home); err != nil {
			t.Errorf("remove test directory: %v", err)
		}
	})
	actualHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	s := &liveServiceSystem{home: home, executable: filepath.Join(home, "errand"), label: "dev.lydakis.errand.test." + filepath.Base(home), unitDir: filepath.Join(actualHome, linuxUnitSubdir)}
	if runtime.GOOS == "linux" && os.Getenv("XDG_CONFIG_HOME") != "" {
		s.unitDir = filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "systemd/user")
	}
	socket := filepath.Join(home, "state", "errand.sock")
	t.Logf("isolated service %s; state %s", s.label, home)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		definition := filepath.Join(home, darwinAgentSubdir, LaunchAgentLabel+".plist")
		if runtime.GOOS == "linux" {
			definition = filepath.Join(s.unitDir, s.label+".service")
		}
		if err := cleanupLiveService(cleanup, s, definition); err != nil {
			t.Errorf("remove isolated service: %v", err)
		}
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
			_, err := s.SocketPID(cleanup, socket)
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Errorf("cleanup left the isolated service socket alive: %s", socket)
	})
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"live-v1", "live-v2"} {
		cmd := exec.CommandContext(ctx, "go", "build", "-ldflags=-X main.version="+version, "-o", filepath.Join(home, "Cellar", "errand", version, "bin", "errand"), "./cmd/errand")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", version, err, out)
		}
	}
	opt := filepath.Join(home, "opt")
	if err := os.MkdirAll(opt, 0755); err != nil {
		t.Fatal(err)
	}
	stablePackage := filepath.Join(opt, "errand")
	if err := os.Symlink("../Cellar/errand/live-v1", stablePackage); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(stablePackage, "bin", "errand"), s.executable); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "daemon.toml")
	networkPeer := os.Getenv("ERRAND_TEST_NETWORK_PEER")
	networkURL := ""
	initial := fmt.Sprintf("transport = 'local'\nstate_dir = %q\nmax_jobs = 2\nmax_queued = 3\n", filepath.Join(home, "state"))
	if networkPeer != "" {
		requireLiveFirewall(t, ctx)
		initial, networkURL = liveNetworkConfig(t, ctx, home)
	}
	if err := os.WriteFile(configPath, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	waitStopped := func() {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
			_, err := s.SocketPID(ctx, socket)
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("test service socket remained live after stop")
	}
	options := Options{ConfigPath: configPath, ExpectedVersion: "live-v1"}
	checkSetup := func() int {
		t.Helper()
		report, err := Run(ctx, options, s)
		if err != nil || report.Failed() {
			t.Fatalf("setup: %v / %+v", err, report.Steps)
		}
		pid, err := s.SocketPID(ctx, socket)
		if err != nil {
			t.Fatal(err)
		}
		if report.Info.Version != options.ExpectedVersion || report.Info.MaxJobs != 2 || report.Info.MaxQueued != 3 || report.Info.LocalOnly != (networkPeer == "") || report.Info.SSHDisabled != (networkPeer == "") || !strings.Contains(stepDetail(report, "probe"), "matches CLI") {
			t.Fatalf("unexpected live info: %+v", report.Info)
		}
		t.Logf("verified %s in service PID %d", report.Info.Version, pid)
		return pid
	}
	firstPID := checkSetup()
	if networkPeer != "" {
		liveRemoteProbe(t, ctx, networkPeer, networkURL, "live-v1")
	}
	servicePath := filepath.Join(home, linuxUnitSubdir, DefaultServiceName+".service")
	if runtime.GOOS == "darwin" {
		servicePath = filepath.Join(home, darwinAgentSubdir, LaunchAgentLabel+".plist")
	}
	originalService, err := s.ReadFile(servicePath)
	if err != nil {
		t.Fatal(err)
	}
	clientDir := filepath.Join(home, ".config", "errand")
	if err := os.MkdirAll(clientDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clientDir, "config.toml"), []byte(fmt.Sprintf("[peers.integration]\nsocket = %q\n", socket)), 0600); err != nil {
		t.Fatal(err)
	}
	job := func(command ...string) *exec.Cmd {
		args := append([]string{"--on", "integration", "--no-snapshot", "--no-apply", "--no-artifacts", "--no-caches", "--"}, command...)
		cmd := exec.CommandContext(ctx, s.executable, args...)
		cmd.Dir = home
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
		return cmd
	}
	if out, err := job("/usr/bin/printf", "live-job-ok").CombinedOutput(); err != nil || !bytes.Contains(out, []byte("live-job-ok")) {
		t.Fatalf("live job: %v / %s", err, out)
	}
	releasePath := filepath.Join(home, "release-busy-job")
	// The job exits only when the test releases it, not after a scheduling-dependent delay.
	busy := job("/bin/sh", "-c", `while [ -d "$2" ] && [ ! -e "$1" ]; do sleep 0.05; done`, "wait-for-release", releasePath, home)
	var busyOutput bytes.Buffer
	if networkPeer != "" {
		busy = liveRemoteBusyJob(ctx, networkPeer, networkURL, releasePath, home)
		busy.Stdout, busy.Stderr = &busyOutput, &busyOutput
	}
	if err := busy.Start(); err != nil {
		t.Fatal(err)
	}
	busyDone := make(chan error, 1)
	go func() { busyDone <- busy.Wait() }()
	waited := false
	defer func() {
		// Release on assertion failures as well, so the remote job cannot be orphaned.
		if err := os.WriteFile(releasePath, nil, 0600); err != nil {
			t.Errorf("release busy job during cleanup: %v", err)
		}
		if !waited {
			select {
			case <-busyDone:
			case <-time.After(5 * time.Second):
				busy.Process.Kill()
				<-busyDone
			}
		}
	}()
	active := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		info, err := s.Probe(ctx, socket)
		if err == nil && info.RunningJobs > 0 {
			active = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !active {
		t.Fatal("test job did not become active")
	}
	// Simulate Homebrew retargeting its opt directory while the daemon runs.
	if err := os.Symlink("../Cellar/errand/live-v2", stablePackage+".new"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stablePackage+".new", stablePackage); err != nil {
		t.Fatal(err)
	}
	// Actual cleanup while the gated job is active. Both generations are
	// candidate builds; this does not claim a released 0.2.1 migration.
	if err := os.RemoveAll(filepath.Join(home, "Cellar", "errand", "live-v1")); err != nil {
		t.Fatal(err)
	}
	if info, err := s.Probe(ctx, socket); err != nil || info.Version != "live-v1" || info.RunningJobs != 1 {
		t.Fatalf("cleanup interrupted active daemon: %+v / %v", info, err)
	}
	if networkPeer != "" {
		liveRemoteProbe(t, ctx, networkPeer, networkURL, "live-v1")
	}
	// Exercise a delay longer than the old two-second job lifetime. The gate
	// must keep the job running until setup has actually refused the restart.
	time.Sleep(2100 * time.Millisecond)
	beforeWrites := s.writes
	refused, err := Run(ctx, options, s)
	if err != nil || !refused.Failed() || !strings.Contains(stepErrorDetail(refused, "service"), "active jobs") || s.writes != beforeWrites {
		t.Fatalf("busy runner was restarted: %v / %+v", err, refused.Steps)
	}
	if err := os.WriteFile(releasePath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-busyDone:
		waited = true
		if err != nil {
			t.Fatalf("active job was interrupted: %v / %s", err, &busyOutput)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("busy job did not finish after release")
	}
	if pid, err := s.SocketPID(ctx, socket); err != nil || pid != firstPID {
		t.Fatalf("busy setup changed process: %d / %v", pid, err)
	}
	if networkPeer != "" && !bytes.Contains(busyOutput.Bytes(), []byte("remote-result-ok")) {
		t.Fatalf("remote result retrieval missing: %s", &busyOutput)
	}
	t.Log("real job completed; busy setup refused without interruption")
	info, err := s.Probe(ctx, socket)
	if err != nil || info.Version != "live-v1" {
		t.Fatalf("disk upgrade unexpectedly changed live process: %+v / %v", info, err)
	}
	options.ExpectedVersion = "live-v2"
	if pid := checkSetup(); pid == firstPID {
		t.Fatal("upgrade kept the old PID")
	}
	if networkPeer != "" {
		liveRemoteProbe(t, ctx, networkPeer, networkURL, "live-v2")
	}
	if upgradedService, err := s.ReadFile(servicePath); err != nil || !bytes.Equal(originalService, upgradedService) {
		t.Fatalf("upgrade changed the service definition: %s / %v", upgradedService, err)
	}
	// Exercise re-enabling an explicitly disabled setup-managed service.
	if runtime.GOOS == "darwin" {
		target := "gui/" + uidString(s.UID()) + "/" + LaunchAgentLabel
		if _, err := s.Run(ctx, "launchctl", "disable", target); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Run(ctx, "launchctl", "bootout", target); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := s.Run(ctx, "systemctl", "--user", "disable", "--now", "errand.service"); err != nil {
			t.Fatal(err)
		}
	}
	waitStopped()
	checkSetup()
	// A separate process owns the same socket: setup must refuse without writing
	// files or reserving that daemon, even though it reports the expected version.
	if runtime.GOOS == "darwin" {
		if _, err := s.Run(ctx, "launchctl", "bootout", "gui/"+uidString(s.UID())+"/"+LaunchAgentLabel); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := s.Run(ctx, "systemctl", "--user", "stop", "errand.service"); err != nil {
			t.Fatal(err)
		}
	}
	waitStopped()
	foreign := exec.CommandContext(ctx, s.executable, "serve", "--config", configPath)
	var foreignLog bytes.Buffer
	foreign.Stdout, foreign.Stderr = &foreignLog, &foreignLog
	if err := foreign.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { foreign.Process.Kill(); foreign.Wait() }()
	ready := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if pid, err := s.SocketPID(ctx, socket); err == nil && pid == foreign.Process.Pid {
			ready = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("foreign daemon did not bind its test socket")
	}
	writes := s.writes
	report, err := Run(ctx, options, s)
	if err != nil || !report.Failed() || s.writes != writes {
		t.Fatalf("foreign daemon accepted or modified: %v / %+v", err, report.Steps)
	}
	info, err = s.Probe(ctx, socket)
	if err != nil || info.Busy {
		t.Fatalf("foreign daemon left quiesced: %+v / %v", info, err)
	}
	if b, err := os.ReadFile(configPath); err != nil || string(b) != initial {
		t.Fatalf("saved config changed: %v / %s", err, b)
	}
	t.Log("foreign daemon rejected; configuration and availability preserved")
}
