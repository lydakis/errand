package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/tailnet"
)

// fakeSystem records every decision setup makes without touching the host.
type fakeSystem struct {
	goos           string
	home           string
	cwd            string
	env            map[string]string
	files          map[string]string
	symlinks       map[string]string
	writable       map[string]bool
	commands       []string
	cmdOutput      map[string]string
	cmdErr         map[string]error
	readErr        map[string]error
	provider       tailnet.Provider
	probeInfo      proto.Info
	probeErr       error
	probeSockets   []string
	quiesceErr     error
	quiesceSockets []string
	releasedLeases []string
	writableChecks []string
	writes         []string
	discoverSocket string
	discoverCLI    string
}

func newFake(t *testing.T, goos string) *fakeSystem {
	t.Helper()
	return &fakeSystem{
		goos: goos, home: "/home/george", cwd: "/work",
		env:   map[string]string{"PATH": "/home/george/.local/bin:relative:/usr/bin"},
		files: map[string]string{}, symlinks: map[string]string{}, writable: map[string]bool{},
		cmdOutput: map[string]string{}, cmdErr: map[string]error{}, readErr: map[string]error{},
		provider: fakeProvider{name: "localapi:/var/run/tailscale/tailscaled.sock", self: tailnet.Self{
			Login: "george@example.com", UserID: 42, DNSName: "cabal.example.ts.net", HostName: "cabal",
			IPs: []string{"100.64.0.9"}, OS: "linux", Version: "1.102.3",
		}},
		probeInfo: proto.Info{Version: "test", MaxJobs: 1, Facts: proto.Facts{OS: "linux", Arch: "amd64", NumCPU: 4}},
	}
}

func (f *fakeSystem) GOOS() string                { return f.goos }
func (f *fakeSystem) Home() (string, error)       { return f.home, nil }
func (f *fakeSystem) Username() string            { return "george" }
func (f *fakeSystem) UID() int                    { return 501 }
func (f *fakeSystem) Executable() (string, error) { return "/home/george/bin/errand", nil }
func (f *fakeSystem) Abs(p string) (string, error) {
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	return filepath.Join(f.cwd, p), nil
}
func (f *fakeSystem) Getenv(key string) string { return f.env[key] }
func (f *fakeSystem) ReadFile(p string) ([]byte, error) {
	if err := f.readErr[p]; err != nil {
		return nil, err
	}
	if s, ok := f.files[p]; ok {
		return []byte(s), nil
	}
	return nil, os.ErrNotExist
}
func (f *fakeSystem) WriteFile(p string, d []byte, _ os.FileMode) error {
	f.writes = append(f.writes, p)
	f.files[p] = string(d)
	return nil
}
func (f *fakeSystem) Exists(p string) bool {
	_, file := f.files[p]
	_, link := f.symlinks[p]
	return file || link
}
func (f *fakeSystem) IsSymlink(p string) bool           { _, ok := f.symlinks[p]; return ok }
func (f *fakeSystem) Readlink(p string) (string, error) { return f.symlinks[p], nil }
func (f *fakeSystem) Symlink(target, link string) error { f.symlinks[link] = target; return nil }
func (f *fakeSystem) Remove(p string) error             { delete(f.symlinks, p); delete(f.files, p); return nil }
func (f *fakeSystem) Writable(dir string) bool {
	f.writableChecks = append(f.writableChecks, dir)
	return f.writable[dir]
}
func (f *fakeSystem) Run(_ context.Context, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	f.commands = append(f.commands, line)
	if err, ok := f.cmdErr[line]; ok {
		return "", err
	}
	return f.cmdOutput[line], nil
}
func (f *fakeSystem) Discover(socket, cli string) (tailnet.Provider, error) {
	f.discoverSocket = socket
	f.discoverCLI = cli
	return f.provider, nil
}
func (f *fakeSystem) Probe(_ context.Context, socket string) (proto.Info, error) {
	f.probeSockets = append(f.probeSockets, socket)
	return f.probeInfo, f.probeErr
}
func (f *fakeSystem) Quiesce(_ context.Context, socket string) (string, error) {
	f.quiesceSockets = append(f.quiesceSockets, socket)
	if f.quiesceErr != nil {
		return "", f.quiesceErr
	}
	if active := testActiveJobSummary(f.probeInfo); active != "" {
		return "", &QuiesceError{Status: 409, Message: "runner has active jobs (" + active + ")"}
	}
	return "test-lease", nil
}

func testActiveJobSummary(info proto.Info) string {
	var active []string
	for _, count := range []struct {
		n int
		s string
	}{
		{info.StagingJobs, "staging"}, {info.StartingJobs, "starting"},
		{info.RunningJobs, "running"}, {info.QueuedJobs, "queued"},
	} {
		if count.n != 0 {
			active = append(active, fmt.Sprintf("%d %s", count.n, count.s))
		}
	}
	return strings.Join(active, ", ")
}
func (f *fakeSystem) ReleaseQuiesce(_ context.Context, socket, token string) error {
	f.releasedLeases = append(f.releasedLeases, socket+"\x00"+token)
	return nil
}

type fakeProvider struct {
	name string
	self tailnet.Self
}

func (p fakeProvider) Name() string { return p.name }
func (p fakeProvider) WhoIs(context.Context, string, string) (tailnet.WhoIs, error) {
	return tailnet.WhoIs{}, errors.New("not used")
}
func (p fakeProvider) SelfIPs(context.Context) ([]string, error)  { return p.self.IPs, nil }
func (p fakeProvider) Self(context.Context) (tailnet.Self, error) { return p.self, nil }

func ran(f *fakeSystem, prefix string) bool {
	for _, c := range f.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func stepDetail(r *Report, name string) string {
	for _, s := range r.Steps {
		if s.Name == name {
			return s.Detail
		}
	}
	return ""
}

func stepErrorDetail(r *Report, name string) string {
	for _, s := range r.Steps {
		if s.Name == name && s.Err != nil {
			return s.Detail
		}
	}
	return ""
}

func TestFreshLinuxRunnerSetup(t *testing.T) {
	f := newFake(t, "linux")
	f.cmdOutput["loginctl show-user george -p Linger"] = "Linger=no"
	f.writable["/usr/local/bin"] = true
	r, err := Run(context.Background(), Options{MaxJobs: 2, AllowUsers: []string{"friend@example.com"}}, f)
	if err != nil || r.Failed() {
		t.Fatalf("setup failed: %v / %+v", err, r.Steps)
	}
	cfg := f.files["/home/george/.config/errand/errandd.toml"]
	for _, want := range []string{`listen = "tailnet:7443"`, "max_jobs = 2",
		`allow_users = ["friend@example.com", "george@example.com"]`,
		`tailscaled_socket = "/var/run/tailscale/tailscaled.sock"`} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("config missing %q:\n%s", want, cfg)
		}
	}
	unit := f.files["/home/george/.config/systemd/user/errand.service"]
	if !strings.Contains(unit, `ExecStart="/home/george/bin/errand" serve --config "/home/george/.config/errand/errandd.toml"`) ||
		!strings.Contains(unit, `Environment="PATH=/home/george/.local/bin:/usr/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/sbin:/bin"`) {
		t.Fatalf("unit = %s", unit)
	}
	for _, cmd := range []string{"systemctl --user daemon-reload", "systemctl --user enable errand.service",
		"systemctl --user restart errand.service", "loginctl enable-linger george"} {
		if !ran(f, cmd) {
			t.Fatalf("expected %q; ran %v", cmd, f.commands)
		}
	}
	if f.symlinks["/usr/local/bin/errand"] != "/home/george/bin/errand" {
		t.Fatalf("path symlink = %v", f.symlinks)
	}
	if r.Info == nil || r.SocketPath != "/home/george/.errand/errand.sock" {
		t.Fatalf("probe/socket = %+v %q", r.Info, r.SocketPath)
	}
	if r.RemoteCommand != "" {
		t.Fatalf("SSH remote command should be unnecessary after linking: %q", r.RemoteCommand)
	}
}

func TestSecondRunIsIdempotentAndKeepsOperatorEdits(t *testing.T) {
	f := newFake(t, "linux")
	f.cmdOutput["loginctl show-user george -p Linger"] = "Linger=yes"
	f.writable["/usr/local/bin"] = false
	if _, err := Run(context.Background(), Options{}, f); err != nil {
		t.Fatal(err)
	}
	// The operator hand-edits the config and the unit.
	f.files["/home/george/.config/errand/errandd.toml"] += "max_queued = 3\n"
	f.files["/home/george/.config/systemd/user/errand.service"] += "# tuned\n"
	f.commands = nil
	r, err := Run(context.Background(), Options{MaxJobs: 4}, f)
	if err != nil || r.Failed() {
		t.Fatalf("rerun failed: %v / %+v", err, r.Steps)
	}
	if !strings.Contains(f.files["/home/george/.config/errand/errandd.toml"], "max_queued = 3") || !r.Existing {
		t.Fatal("rerun clobbered the operator's config")
	}
	if !strings.HasSuffix(f.files["/home/george/.config/systemd/user/errand.service"], "# tuned\n") {
		t.Fatal("rerun clobbered the operator's unit")
	}
	if !ran(f, "systemctl --user restart errand.service") || ran(f, "loginctl enable-linger") {
		t.Fatalf("rerun did not reload the preserved config cleanly: %v", f.commands)
	}
	if !strings.Contains(stepDetail(r, "path"), "remote_command = /home/george/bin/errand") {
		t.Fatalf("unwritable /usr/local/bin should explain remote_command: %q", stepDetail(r, "path"))
	}
	if r.RemoteCommand != "/home/george/bin/errand" {
		t.Fatalf("SSH remote command = %q", r.RemoteCommand)
	}
}

func TestSetupRefusesToRestartRunnerWithActiveJobs(t *testing.T) {
	tests := []struct {
		name       string
		goos       string
		info       proto.Info
		forbidden  string
		wantDetail string
	}{
		{
			name: "linux running", goos: "linux",
			info:      proto.Info{StagingJobs: 1, RunningJobs: 1},
			forbidden: "systemctl --user restart errand.service", wantDetail: "1 staging, 1 running",
		},
		{
			name: "darwin queued and starting", goos: "darwin",
			info:      proto.Info{QueuedJobs: 2, StartingJobs: 1},
			forbidden: "launchctl bootout", wantDetail: "1 starting, 2 queued",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t, tt.goos)
			f.probeInfo = tt.info
			r, err := Run(context.Background(), Options{}, f)
			if err != nil {
				t.Fatal(err)
			}
			refusal := stepErrorDetail(r, "service")
			if !r.Failed() || !strings.Contains(refusal, tt.wantDetail) {
				t.Fatalf("active runner was not refused: %+v", r.Steps)
			}
			if ran(f, tt.forbidden) {
				t.Fatalf("setup restarted a runner with active jobs: %v", f.commands)
			}
			if len(f.writes) != 0 {
				t.Fatalf("setup wrote files before refusing the active runner: %v", f.writes)
			}
		})
	}
}

func TestSetupReleasesQuiesceWhenServiceUpdateFails(t *testing.T) {
	f := newFake(t, "linux")
	f.cmdErr["systemctl --user daemon-reload"] = errors.New("systemd unavailable")

	r, err := Run(context.Background(), Options{}, f)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Failed() || len(f.releasedLeases) != 1 {
		t.Fatalf("failed setup did not release its lease: steps=%+v releases=%v", r.Steps, f.releasedLeases)
	}
}

func TestSetupChecksTheExistingRunnerSocketBeforeForcedRewrite(t *testing.T) {
	f := newFake(t, "linux")
	configPath := "/home/george/.config/errand/errandd.toml"
	f.files[configPath] = "listen = \"tailnet:7443\"\nsocket = \"/tmp/existing-errand.sock\"\nmax_jobs = 1\n"
	f.files["/home/george/.config/systemd/user/errand.service"] = "operator service\n"
	originalConfig := f.files[configPath]
	f.probeInfo.RunningJobs = 1

	r, err := Run(context.Background(), Options{Force: true}, f)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Failed() || len(f.quiesceSockets) != 1 || f.quiesceSockets[0] != "/tmp/existing-errand.sock" {
		t.Fatalf("forced setup checked sockets %v; report: %+v", f.quiesceSockets, r.Steps)
	}
	if ran(f, "systemctl --user restart errand.service") {
		t.Fatalf("forced setup restarted the active runner: %v", f.commands)
	}
	if f.files[configPath] != originalConfig || f.files["/home/george/.config/systemd/user/errand.service"] != "operator service\n" {
		t.Fatalf("refused setup mutated definitions: %v", f.files)
	}
}

func TestSetupRefusesAnActiveServiceWhenTheSelectedSocketIsAbsent(t *testing.T) {
	tests := []struct {
		name      string
		goos      string
		service   string
		activeCmd string
		restart   string
	}{
		{"linux", "linux", "/home/george/.config/systemd/user/errand.service", "systemctl --user is-active errand.service", "systemctl --user restart errand.service"},
		{"darwin", "darwin", "/home/george/Library/LaunchAgents/dev.lydakis.errand.plist", "launchctl print gui/501/dev.lydakis.errand", "launchctl bootout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t, tt.goos)
			f.files[tt.service] = "operator service using another config\n"
			f.quiesceErr = os.ErrNotExist
			f.cmdOutput[tt.activeCmd] = "active"

			r, err := Run(context.Background(), Options{Force: true}, f)
			if err != nil {
				t.Fatal(err)
			}
			if !r.Failed() || !strings.Contains(stepErrorDetail(r, "service"), "active service") {
				t.Fatalf("uninspected active service was not refused: %+v", r.Steps)
			}
			if len(f.writes) != 0 || ran(f, tt.restart) {
				t.Fatalf("setup mutated an uninspected active service: writes=%v commands=%v", f.writes, f.commands)
			}
		})
	}
}

func TestSetupRefusesRestartWhenAnExistingRunnerCannotBeInspected(t *testing.T) {
	f := newFake(t, "linux")
	socket := "/home/george/.errand/errand.sock"
	f.files[socket] = "socket"
	f.quiesceErr = errors.New("permission denied")

	r, err := Run(context.Background(), Options{}, f)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Failed() || !strings.Contains(stepErrorDetail(r, "service"), "cannot reserve") {
		t.Fatalf("uninspectable runner was not refused: %+v", r.Steps)
	}
	if ran(f, "systemctl --user restart errand.service") {
		t.Fatalf("setup restarted an uninspectable runner: %v", f.commands)
	}
}

func TestSetupIgnoresAStaleRunnerSocket(t *testing.T) {
	f := newFake(t, "linux")
	socket := "/home/george/.errand/errand.sock"
	f.files[socket] = "stale socket"
	f.quiesceErr = syscall.ECONNREFUSED
	f.cmdOutput["loginctl show-user george -p Linger"] = "Linger=yes"

	r, err := Run(context.Background(), Options{}, f)
	if err != nil || r.Failed() {
		t.Fatalf("stale socket blocked setup: %v / %+v", err, r.Steps)
	}
	if !ran(f, "systemctl --user restart errand.service") {
		t.Fatalf("setup did not recover past stale socket: %v", f.commands)
	}
}

func TestForceRewritesDefinitionsAndDryRunChangesNothing(t *testing.T) {
	f := newFake(t, "linux")
	f.files["/home/george/.config/errand/errandd.toml"] = "listen = \"tailnet:9999\"\n"
	f.files["/home/george/.config/systemd/user/errand.service"] = "stale\n"
	r, err := Run(context.Background(), Options{DryRun: true, Force: true}, f)
	if err != nil || r.Failed() {
		t.Fatalf("dry run failed: %v / %+v", err, r.Steps)
	}
	if f.files["/home/george/.config/errand/errandd.toml"] != "listen = \"tailnet:9999\"\n" || len(f.commands) != 0 {
		t.Fatalf("dry run changed the system: files=%v commands=%v", f.files, f.commands)
	}
	if !strings.Contains(stepDetail(r, "config"), "would write") {
		t.Fatalf("dry run should describe the config: %q", stepDetail(r, "config"))
	}
	r, err = Run(context.Background(), Options{Force: true}, f)
	if err != nil || r.Failed() {
		t.Fatalf("force run failed: %v / %+v", err, r.Steps)
	}
	if !strings.Contains(f.files["/home/george/.config/errand/errandd.toml"], `listen = "tailnet:7443"`) ||
		f.files["/home/george/.config/systemd/user/errand.service"] == "stale\n" {
		t.Fatal("--force did not rewrite the definitions")
	}
}

func TestForceRestartsLinuxServiceWhenOnlyConfigChanges(t *testing.T) {
	f := newFake(t, "linux")
	f.cmdOutput["loginctl show-user george -p Linger"] = "Linger=yes"
	if _, err := Run(context.Background(), Options{}, f); err != nil {
		t.Fatal(err)
	}
	f.commands = nil

	r, err := Run(context.Background(), Options{Force: true, MaxJobs: 4}, f)
	if err != nil || r.Failed() {
		t.Fatalf("forced config update failed: %v / %+v", err, r.Steps)
	}
	if !ran(f, "systemctl --user restart errand.service") {
		t.Fatalf("config-only update did not restart the service: %v", f.commands)
	}
}

func TestSetupKeepsAnExistingPathEntryWithoutForce(t *testing.T) {
	f := newFake(t, "linux")
	f.cmdOutput["loginctl show-user george -p Linger"] = "Linger=yes"
	f.writable["/usr/local/bin"] = true
	f.symlinks["/usr/local/bin/errand"] = "/opt/managed/errand"

	r, err := Run(context.Background(), Options{}, f)
	if err != nil || r.Failed() {
		t.Fatalf("setup failed: %v / %+v", err, r.Steps)
	}
	if got := f.symlinks["/usr/local/bin/errand"]; got != "/opt/managed/errand" {
		t.Fatalf("setup replaced an existing PATH entry: %q", got)
	}
	if !strings.Contains(stepDetail(r, "path"), "left alone") {
		t.Fatalf("path decision = %q", stepDetail(r, "path"))
	}
}

func TestSetupRecognizesRelativeSymlinkToCurrentExecutable(t *testing.T) {
	f := newFake(t, "linux")
	f.cmdOutput["loginctl show-user george -p Linger"] = "Linger=yes"
	f.symlinks["/usr/local/bin/errand"] = "../../../home/george/bin/errand"

	r, err := Run(context.Background(), Options{}, f)
	if err != nil || r.Failed() {
		t.Fatalf("setup failed: %v / %+v", err, r.Steps)
	}
	if got := f.symlinks["/usr/local/bin/errand"]; got != "../../../home/george/bin/errand" {
		t.Fatalf("setup rewrote the matching relative symlink: %q", got)
	}
}

func TestDryRunDoesNotChangeThePathEntry(t *testing.T) {
	f := newFake(t, "linux")
	f.writable["/usr/local/bin"] = true
	f.symlinks["/usr/local/bin/errand"] = "/opt/managed/errand"

	r, err := Run(context.Background(), Options{DryRun: true, Force: true}, f)
	if err != nil || r.Failed() {
		t.Fatalf("dry run failed: %v / %+v", err, r.Steps)
	}
	if got := f.symlinks["/usr/local/bin/errand"]; got != "/opt/managed/errand" {
		t.Fatalf("dry run changed the PATH entry: %q", got)
	}
	if !strings.Contains(stepDetail(r, "path"), "would replace") {
		t.Fatalf("path decision = %q", stepDetail(r, "path"))
	}
	if len(f.writableChecks) != 0 || len(f.quiesceSockets) != 0 {
		t.Fatalf("dry run performed mutating probes: writable=%v quiesce=%v", f.writableChecks, f.quiesceSockets)
	}
}

func TestExistingConfigDrivesTheEffectiveReport(t *testing.T) {
	f := newFake(t, "linux")
	f.cmdOutput["loginctl show-user george -p Linger"] = "Linger=yes"
	path := "/home/george/.config/errand/errandd.toml"
	f.files[path] = "listen = \"none\"\nsocket = \"/tmp/custom.sock\"\nmax_jobs = 3\nallow_users = [\"other@example.com\"]\ndeny_users = [\"other@example.com\"]\n"

	r, err := Run(context.Background(), Options{}, f)
	if err != nil || r.Failed() {
		t.Fatalf("setup failed: %v / %+v", err, r.Steps)
	}
	if r.Config.Listen != "none" || r.Config.MaxJobs != 3 ||
		len(r.Config.AllowUsers) != 1 || r.Config.AllowUsers[0] != "other@example.com" ||
		len(r.Config.DenyUsers) != 1 || r.Config.DenyUsers[0] != "other@example.com" {
		t.Fatalf("effective config = %+v", r.Config)
	}
	if r.SocketPath != "/tmp/custom.sock" {
		t.Fatalf("socket path = %q", r.SocketPath)
	}
}

func TestRelativeConfigPathIsMadeAbsoluteForTheService(t *testing.T) {
	f := newFake(t, "linux")
	f.cmdOutput["loginctl show-user george -p Linger"] = "Linger=yes"

	r, err := Run(context.Background(), Options{ConfigPath: "config/runner.toml"}, f)
	if err != nil || r.Failed() {
		t.Fatalf("setup failed: %v / %+v", err, r.Steps)
	}
	want := "/work/config/runner.toml"
	if r.ConfigPath != want || !f.Exists(want) {
		t.Fatalf("config path = %q, files = %v", r.ConfigPath, f.files)
	}
	unit := f.files["/home/george/.config/systemd/user/errand.service"]
	if !strings.Contains(unit, `--config "/work/config/runner.toml"`) {
		t.Fatalf("service retained a relative config path:\n%s", unit)
	}
}

func TestUnreadableExistingConfigStopsSetup(t *testing.T) {
	f := newFake(t, "linux")
	path := "/home/george/.config/errand/errandd.toml"
	f.files[path] = "listen = \"none\"\n"
	f.readErr[path] = errors.New("permission denied")

	r, err := Run(context.Background(), Options{}, f)
	if err != nil {
		t.Fatal(err)
	}
	failedDetail := ""
	for _, step := range r.Steps {
		if step.Name == "config" && step.Err != nil {
			failedDetail = step.Detail
		}
	}
	if !r.Failed() || !strings.Contains(failedDetail, "permission denied") {
		t.Fatalf("unreadable config was not reported: %+v", r.Steps)
	}
	if len(f.commands) != 0 {
		t.Fatalf("setup continued after an unreadable config: %v", f.commands)
	}
}

func TestExistingConfigSuppliesTheIdentityProviderOnRerun(t *testing.T) {
	f := newFake(t, "darwin")
	path := "/home/george/.config/errand/errandd.toml"
	f.files[path] = "listen = \"tailnet:7443\"\nmax_jobs = 1\ntailscale_cli = \"/Applications/Tailscale.app/Contents/MacOS/Tailscale\"\n"

	r, err := Run(context.Background(), Options{}, f)
	if err != nil || r.Failed() {
		t.Fatalf("setup rerun failed: %v / %+v", err, r.Steps)
	}
	if f.discoverCLI != "/Applications/Tailscale.app/Contents/MacOS/Tailscale" || f.discoverSocket != "" {
		t.Fatalf("provider discovery used socket=%q cli=%q", f.discoverSocket, f.discoverCLI)
	}
}

func TestDarwinInstallsLaunchAgentAndUsesCLIProvider(t *testing.T) {
	f := newFake(t, "darwin")
	f.home = "/Users/george"
	f.provider = fakeProvider{name: "cli:/usr/local/bin/tailscale", self: tailnet.Self{
		Login: "george@example.com", UserID: 42, DNSName: "mini.example.ts.net", HostName: "mini", OS: "macOS", Version: "1.102.3",
	}}
	f.writable["/usr/local/bin"] = true
	logPath := "/Users/george/Library/Logs/errand/errand.log"
	f.files[logPath] = "existing diagnostics\n"
	r, err := Run(context.Background(), Options{}, f)
	if err != nil || r.Failed() {
		t.Fatalf("darwin setup failed: %v / %+v", err, r.Steps)
	}
	cfg := f.files["/Users/george/.config/errand/errandd.toml"]
	if !strings.Contains(cfg, `tailscale_cli = "/usr/local/bin/tailscale"`) || strings.Contains(cfg, "tailscaled_socket") {
		t.Fatalf("darwin config should pin the CLI provider:\n%s", cfg)
	}
	plist := f.files["/Users/george/Library/LaunchAgents/dev.lydakis.errand.plist"]
	if !strings.Contains(plist, "<string>/home/george/bin/errand</string>") || !strings.Contains(plist, "<string>serve</string>") ||
		!strings.Contains(plist, "<key>PATH</key>") ||
		!strings.Contains(plist, "<string>/home/george/.local/bin:/usr/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/sbin:/bin</string>") {
		t.Fatalf("plist = %s", plist)
	}
	if f.files[logPath] != "existing diagnostics\n" {
		t.Fatalf("setup truncated the existing launch-agent log: %q", f.files[logPath])
	}
	if !ran(f, "launchctl bootstrap gui/501 /Users/george/Library/LaunchAgents/dev.lydakis.errand.plist") {
		t.Fatalf("launch agent not bootstrapped: %v", f.commands)
	}
	if r.Service != LaunchAgentLabel {
		t.Fatalf("service = %q", r.Service)
	}
}

func TestOldTailscaleFailsClosedBeforeTouchingAnything(t *testing.T) {
	f := newFake(t, "linux")
	p := f.provider.(fakeProvider)
	p.self.Version = "1.98.0"
	f.provider = p
	_, err := Run(context.Background(), Options{}, f)
	if err == nil || !strings.Contains(err.Error(), "1.100") {
		t.Fatalf("old tailscaled must refuse setup: %v", err)
	}
	if len(f.files) != 0 || len(f.commands) != 0 {
		t.Fatalf("refusal must change nothing: %v %v", f.files, f.commands)
	}
}

func TestProbeFailureIsReportedNotHidden(t *testing.T) {
	f := newFake(t, "linux")
	f.cmdOutput["loginctl show-user george -p Linger"] = "Linger=yes"
	f.probeErr = errors.New("connection refused")
	r, err := Run(context.Background(), Options{}, f)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Failed() || !strings.Contains(stepDetail(r, "probe"), "did not answer") {
		t.Fatalf("probe failure not reported: %+v", r.Steps)
	}
}

func TestRenderACLNamesCapabilityAndPort(t *testing.T) {
	acl := RenderACL("example.com/cap/errand", 7443)
	for _, want := range []string{
		`"tcp:7443"`, `"example.com/cap/errand"`, "tag:errand-runner",
		proto.ActionForwardOwn, proto.ActionCaches, proto.ActionGCJobs,
	} {
		if !strings.Contains(acl, want) {
			t.Fatalf("acl missing %q:\n%s", want, acl)
		}
	}
}

func TestGeneratedConfigDescribesAllowUsersAsFullAccess(t *testing.T) {
	cfg := renderConfig(ConfigChoice{Listen: "tailnet:7443", MaxJobs: 1, AllowUsers: []string{"george@example.com"}})
	if !strings.Contains(cfg, "full runner access") {
		t.Fatalf("generated allow_users documentation understates its authority:\n%s", cfg)
	}
}

func TestServiceDefinitionsEscapePaths(t *testing.T) {
	unit := renderSystemdUnit("/opt/Errand Tools/errand", "/home/george/a%b/config.toml", "/opt/A%B/bin:/usr/bin")
	if !strings.Contains(unit, `ExecStart="/opt/Errand Tools/errand" serve --config "/home/george/a%%b/config.toml"`) {
		t.Fatalf("systemd unit did not quote paths:\n%s", unit)
	}
	if !strings.Contains(unit, `Environment="PATH=/opt/A%%B/bin:/usr/bin"`) {
		t.Fatalf("systemd unit did not quote PATH:\n%s", unit)
	}

	plist := renderLaunchAgent("dev.example.errand", "/Applications/A&B/errand", "/Users/g/a&b.toml", "/tmp/a&b.log", "/Applications/A&B/bin:/usr/bin")
	for _, want := range []string{"/Applications/A&amp;B/errand", "/Users/g/a&amp;b.toml", "/tmp/a&amp;b.log", "/Applications/A&amp;B/bin:/usr/bin"} {
		if !strings.Contains(plist, want) {
			t.Fatalf("launch agent missing escaped %q:\n%s", want, plist)
		}
	}
}

func TestServicePathKeepsAbsoluteEntriesAndAddsSystemDefaults(t *testing.T) {
	got := servicePath("relative:/opt/homebrew/bin::/usr/bin:/opt/homebrew/bin")
	want := "/opt/homebrew/bin:/usr/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/sbin:/bin"
	if got != want {
		t.Fatalf("service PATH = %q, want %q", got, want)
	}
}

func (fakeProvider) Peers(context.Context) ([]tailnet.Peer, error) { return nil, nil }
