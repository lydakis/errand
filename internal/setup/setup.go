package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/tailnet"
)

const (
	DefaultPort         = 7443
	DefaultServiceName  = "errand"
	LaunchAgentLabel    = "dev.lydakis.errand"
	pathSymlinkDir      = "/usr/local/bin"
	probeTimeout        = 8 * time.Second
	probeInterval       = 250 * time.Millisecond
	linuxUnitSubdir     = ".config/systemd/user"
	darwinAgentSubdir   = "Library/LaunchAgents"
	darwinLogSubdir     = "Library/Logs/errand"
	systemdNotAvailable = "systemctl --user is unavailable; install a service manually"
)

// Options are the operator's choices; everything else is discovered.
type Options struct {
	Transport  string // empty respects saved config; new runners default to both
	ConfigPath string // empty uses the default runner path, honoring XDG_CONFIG_HOME
	MaxJobs    int    // zero uses 1
	AllowUsers []string
	Socket     string // explicit tailscaled socket
	CLI        string // explicit tailscale CLI path
	Force      bool   // rewrite an existing config or service definition
	DryRun     bool   // decide and report, change nothing
}

// ConfigChoice is the runner config setup decided to write.
type ConfigChoice struct {
	Transport        string
	Listen           string
	MaxJobs          int
	AllowUsers       []string
	DenyUsers        []string
	TailscaledSocket string
	TailscaleCLI     string
}

// Step is one decision with its outcome; the report is the list of steps.
type Step struct {
	Name    string
	Detail  string
	Changed bool
	Err     error
}

type Report struct {
	Steps      []Step
	Executable string
	// RemoteCommand is empty when setup proved that a non-interactive SSH
	// session can resolve errand through /usr/local/bin.
	RemoteCommand string
	Provider      string
	Self          tailnet.Self
	ConfigPath    string
	Config        ConfigChoice
	Existing      bool // config existed and was kept
	Service       string
	SocketPath    string
	Info          *proto.Info
}

func (r *Report) step(name, detail string, changed bool) {
	r.Steps = append(r.Steps, Step{Name: name, Detail: detail, Changed: changed})
}

func (r *Report) fail(name string, err error) {
	r.Steps = append(r.Steps, Step{Name: name, Detail: err.Error(), Err: err})
}

// Failed reports whether any step recorded an error.
func (r *Report) Failed() bool {
	for _, s := range r.Steps {
		if s.Err != nil {
			return true
		}
	}
	return false
}

// Run performs the setup. It stops at the first error that makes later
// steps meaningless (unavailable identity provider, no home) and otherwise records
// per-step outcomes so the operator sees everything at once.
func Run(ctx context.Context, opts Options, sys System) (*Report, error) {
	r := &Report{}
	home, err := sys.Home()
	if err != nil {
		return r, err
	}
	configPath := opts.ConfigPath
	if configPath == "" {
		configDir, err := config.Directory(home, sys.Getenv("XDG_CONFIG_HOME"))
		if err != nil {
			return r, err
		}
		configPath = filepath.Join(configDir, "errandd.toml")
		// Released setup versions ignored XDG_CONFIG_HOME. Do not mistake an
		// existing installation for a fresh one and change its transport policy.
		legacyPath := filepath.Join(home, ".config", "errand", "errandd.toml")
		if configPath != legacyPath && !sys.Exists(configPath) && sys.Exists(legacyPath) {
			r.ConfigPath = configPath
			r.fail("config", fmt.Errorf("existing runner config found at %s, but the XDG config %s is missing; rerun errand setup --config %q to keep using the existing configuration", legacyPath, configPath, legacyPath))
			return r, nil
		}
	} else {
		configPath, err = sys.Abs(configPath)
		if err != nil {
			return r, fmt.Errorf("resolving config path %q: %w", opts.ConfigPath, err)
		}
	}
	r.ConfigPath = configPath
	saved := savedConfig{exists: sys.Exists(configPath)}
	if saved.exists {
		data, readErr := sys.ReadFile(configPath)
		if readErr != nil {
			r.fail("config", fmt.Errorf("reading %s: %w", configPath, readErr))
			return r, nil
		}
		saved.raw = data
		d := config.Daemon{MaxJobs: 1, MaxQueued: 8}
		if decodeErr := toml.Unmarshal(data, &d); decodeErr != nil {
			if !opts.Force {
				r.fail("config", fmt.Errorf("reading %s: %w", configPath, decodeErr))
				return r, nil
			}
		} else {
			saved.daemon = &d
		}
	}
	restartSocketPath := filepath.Join(home, ".errand", "errand.sock")
	if saved.daemon != nil {
		// Socket identity is independent of transport validation, including
		// when an explicit override repairs an invalid saved mode.
		previous := *saved.daemon
		if previous.StateDir == "" {
			previous.StateDir = filepath.Join(home, ".errand")
		}
		restartSocketPath = previous.SocketPath()
	}
	exe, err := sys.Executable()
	if err != nil {
		return r, fmt.Errorf("locating the errand binary: %w", err)
	}
	r.Executable = exe
	r.RemoteCommand = exe
	runnerPath := servicePath(sys.Getenv("PATH"))

	// 1. Resolve transport availability and plan the configuration before
	// acquiring a restart lease, writing files, or changing the service.
	transport, err := resolveTransport(ctx, opts, sys, saved, r)
	if err != nil {
		return r, err
	}
	r.Existing = saved.daemon != nil && !opts.Force
	plan, err := reconcileConfig(opts, saved, transport)
	if err != nil {
		return r, err
	}
	effective, err := normalizeDaemonConfig(home, plan.effective)
	if err != nil {
		r.fail("config", fmt.Errorf("reading effective config: %w", err))
		return r, nil
	}

	if effective.Transport == config.TransportLocal && !opts.Force {
		if err := preflightLocalService(sys, home, exe, configPath, runnerPath); err != nil {
			r.fail("service", err)
			return r, nil
		}
	}

	var leaseToken string
	restartCompleted := false
	if !opts.DryRun && (sys.GOOS() == "linux" || sys.GOOS() == "darwin") {
		var ok bool
		leaseToken, ok = acquireRestartLease(ctx, sys, r, restartSocketPath)
		if !ok {
			return r, nil
		}
		if leaseToken == "" {
			active, activeErr := serviceActive(ctx, sys)
			if activeErr != nil {
				r.fail("service", fmt.Errorf("cannot determine whether the installed service is active: %w", activeErr))
				return r, nil
			}
			if active {
				r.fail("service", fmt.Errorf("an active service is not answering on %s; refuse to restart it because its jobs cannot be inspected", restartSocketPath))
				return r, nil
			}
		}
		defer func() {
			if leaseToken == "" || restartCompleted {
				return
			}
			releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = sys.ReleaseQuiesce(releaseCtx, restartSocketPath, leaseToken)
		}()
	}

	// 2. Save only intentional mode changes or newly available transports.
	switch {
	case !plan.changed:
		r.step("config", "kept existing "+configPath+" (use --force to rewrite it)", false)
	case opts.DryRun:
		r.step("config", "would write "+configPath+":\n"+indent(plan.rendered), true)
	default:
		if saved.exists {
			current, readErr := sys.ReadFile(configPath)
			if readErr != nil || !bytes.Equal(current, saved.raw) {
				r.fail("config", fmt.Errorf("config changed during setup; rerun setup"))
				return r, nil
			}
		}
		if err := sys.WriteFile(configPath, []byte(plan.rendered), 0o600); err != nil {
			r.fail("config", err)
			return r, nil
		}
		r.step("config", "wrote "+configPath, true)
	}
	r.Config = ConfigChoice{
		Transport: effective.Transport, Listen: effective.Listen, MaxJobs: effective.MaxJobs,
		AllowUsers: effective.AllowUsers, DenyUsers: effective.DenyUsers, TailscaledSocket: effective.TailscaledSocket,
		TailscaleCLI: effective.TailscaleCLI,
	}
	r.SocketPath = effective.SocketPath()

	// 3. Service.
	switch sys.GOOS() {
	case "linux":
		restartCompleted = installSystemd(ctx, opts, sys, r, home, exe, configPath, runnerPath)
	case "darwin":
		restartCompleted = installLaunchAgent(ctx, opts, sys, r, home, exe, configPath, runnerPath)
	default:
		r.step("service", "no service manager integration for "+sys.GOOS()+"; run `"+exe+" serve` yourself", false)
	}

	// 4. PATH for SSH callers.
	if effective.Transport != config.TransportTailscale && effective.Transport != config.TransportLocal {
		ensureOnPath(sys, r, exe, opts.Force, opts.DryRun)
	}

	// 5. Prove it.
	if opts.DryRun {
		r.step("probe", "skipped (dry run)", false)
		return r, nil
	}
	if r.Failed() {
		return r, nil
	}
	probe(ctx, sys, r)
	return r, nil
}

type serviceSystem interface {
	GOOS() string
	UID() int
	Run(context.Context, string, ...string) (string, error)
}

func serviceActive(ctx context.Context, sys serviceSystem) (bool, error) {
	switch sys.GOOS() {
	case "linux":
		out, err := sys.Run(ctx, "systemctl", "--user", "is-active", DefaultServiceName+".service")
		state := strings.TrimSpace(out)
		switch state {
		case "active", "activating", "reloading", "deactivating":
			return true, nil
		case "inactive", "failed", "unknown":
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	case "darwin":
		domain := "gui/" + uidString(sys.UID()) + "/" + LaunchAgentLabel
		out, err := sys.Run(ctx, "launchctl", "print", domain)
		if err == nil {
			return true, nil
		}
		detail := strings.ToLower(out + " " + err.Error())
		if strings.Contains(detail, "could not find service") || strings.Contains(detail, "service not found") {
			return false, nil
		}
		return false, err
	default:
		return false, nil
	}
}

func installSystemd(ctx context.Context, opts Options, sys System, r *Report, home, exe, configPath, runnerPath string) bool {
	r.Service = DefaultServiceName + ".service"
	unitPath := filepath.Join(home, linuxUnitSubdir, r.Service)
	desired := renderSystemdUnit(exe, configPath, runnerPath)
	changed, ok := writeDefinition(sys, r, "service", unitPath, desired, opts)
	if !ok {
		return false
	}
	if opts.DryRun {
		r.step("service", "would run: systemctl --user daemon-reload && systemctl --user enable "+r.Service+" && systemctl --user restart "+r.Service, true)
		r.step("linger", "would run: loginctl enable-linger "+sys.Username(), true)
		return false
	}
	if _, err := sys.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		r.fail("service", errors.New(systemdNotAvailable+": "+err.Error()))
		return false
	}
	if _, err := sys.Run(ctx, "systemctl", "--user", "enable", r.Service); err != nil {
		r.fail("service", err)
		return false
	}
	if _, err := sys.Run(ctx, "systemctl", "--user", "restart", r.Service); err != nil {
		r.fail("service", err)
		return false
	}
	if changed {
		r.step("service", "installed and started "+r.Service, true)
	} else {
		r.step("service", "restarted "+r.Service+" so the preserved config is active", true)
	}
	// Linger keeps the user service alive after logout and across reboots.
	out, err := sys.Run(ctx, "loginctl", "show-user", sys.Username(), "-p", "Linger")
	if err == nil && strings.TrimSpace(out) == "Linger=yes" {
		r.step("linger", "already enabled for "+sys.Username(), false)
		return true
	}
	if _, err := sys.Run(ctx, "loginctl", "enable-linger", sys.Username()); err != nil {
		r.fail("linger", fmt.Errorf("could not enable linger (the runner will stop at logout): %w; run: sudo loginctl enable-linger %s", err, sys.Username()))
		return true
	}
	r.step("linger", "enabled for "+sys.Username()+" (runner survives logout and reboot)", true)
	return true
}

func installLaunchAgent(ctx context.Context, opts Options, sys System, r *Report, home, exe, configPath, runnerPath string) bool {
	r.Service = LaunchAgentLabel
	plistPath := filepath.Join(home, darwinAgentSubdir, LaunchAgentLabel+".plist")
	logPath := filepath.Join(home, darwinLogSubdir, "errand.log")
	desired := renderLaunchAgent(LaunchAgentLabel, exe, configPath, logPath, runnerPath)
	changed, ok := writeDefinition(sys, r, "service", plistPath, desired, opts)
	if !ok {
		return false
	}
	domain := "gui/" + uidString(sys.UID())
	if opts.DryRun {
		r.step("service", "would run: launchctl bootstrap "+domain+" "+plistPath, true)
		return false
	}
	if !sys.Exists(logPath) {
		if err := sys.WriteFile(logPath, nil, 0o600); err != nil {
			r.fail("service", fmt.Errorf("creating log file: %w", err))
			return false
		}
	}
	// bootout is idempotent (ignored when not loaded); bootstrap loads and
	// starts. Together they make re-running setup a restart.
	_, _ = sys.Run(ctx, "launchctl", "bootout", domain+"/"+LaunchAgentLabel)
	if _, err := sys.Run(ctx, "launchctl", "bootstrap", domain, plistPath); err != nil {
		r.fail("service", err)
		return false
	}
	if changed {
		r.step("service", "installed and started launch agent "+LaunchAgentLabel, true)
	} else {
		r.step("service", "launch agent "+LaunchAgentLabel+" restarted", true)
	}
	return true
}

func acquireRestartLease(ctx context.Context, sys System, r *Report, socketPath string) (string, bool) {
	token, err := sys.Quiesce(ctx, socketPath)
	if err == nil {
		return token, true
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return "", true
	}
	var quiesceErr *QuiesceError
	if errors.As(err, &quiesceErr) && quiesceErr.Status == 409 {
		r.fail("service", errors.New(quiesceErr.Message))
		return "", false
	}
	r.fail("service", fmt.Errorf("cannot reserve the idle runner through %s: %w", socketPath, err))
	return "", false
}

// writeDefinition installs a service definition file unless an operator's
// differing file is present and --force is absent. It returns whether the
// file changed and whether the caller may continue.
func writeDefinition(sys System, r *Report, name, path, desired string, opts Options) (changed, ok bool) {
	if sys.Exists(path) {
		current, err := sys.ReadFile(path)
		if err == nil && string(current) == desired {
			r.step(name, "definition unchanged at "+path, false)
			return false, true
		}
		if !opts.Force {
			r.step(name, "kept existing "+path+" (differs from what setup would write; use --force to replace)", false)
			return false, true
		}
	}
	if opts.DryRun {
		r.step(name, "would write "+path, true)
		return true, true
	}
	if err := sys.WriteFile(path, []byte(desired), 0o644); err != nil {
		r.fail(name, err)
		return false, false
	}
	r.step(name, "wrote "+path, true)
	return true, true
}

// ensureOnPath makes `errand` resolvable from a non-interactive SSH shell,
// whose PATH is typically /usr/local/bin:/usr/bin:/bin. Without this, SSH
// peers need remote_command set to the binary's absolute path.
func ensureOnPath(sys System, r *Report, exe string, force, dryRun bool) {
	link := filepath.Join(pathSymlinkDir, "errand")
	if filepath.Dir(exe) == pathSymlinkDir {
		r.RemoteCommand = ""
		r.step("path", "binary already lives in "+pathSymlinkDir, false)
		return
	}
	if sys.Exists(link) {
		if sys.IsSymlink(link) {
			target, err := sys.Readlink(link)
			if err == nil && symlinkTargetPath(link, target) == filepath.Clean(exe) {
				r.RemoteCommand = ""
				r.step("path", link+" points to "+exe, false)
				return
			}
			if !force {
				r.step("path", link+" points elsewhere; left alone. SSH peers need remote_command = "+exe, false)
				return
			}
			if dryRun {
				r.step("path", "would replace "+link+" with a link to "+exe, true)
				return
			}
			if !sys.Writable(pathSymlinkDir) {
				r.step("path", link+" points elsewhere and "+pathSymlinkDir+" is not writable; SSH peers need remote_command = "+exe, false)
				return
			}
			_ = sys.Remove(link)
		} else {
			r.step("path", link+" exists and is not a symlink; left alone. SSH peers need remote_command = "+exe, false)
			return
		}
	}
	if dryRun {
		r.step("path", "would link "+link+" to "+exe, true)
		return
	}
	if !sys.Writable(pathSymlinkDir) {
		r.step("path", pathSymlinkDir+" is not writable; SSH peers need remote_command = "+exe+
			" (or: sudo ln -s "+exe+" "+link+")", false)
		return
	}
	if err := sys.Symlink(exe, link); err != nil {
		r.step("path", "could not link "+link+": "+err.Error()+"; SSH peers need remote_command = "+exe, false)
		return
	}
	r.RemoteCommand = ""
	r.step("path", "linked "+link+" to "+exe+" (SSH callers find errand on PATH)", true)
}

func symlinkTargetPath(link, target string) string {
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	return filepath.Clean(target)
}

func probe(ctx context.Context, sys System, r *Report) {
	deadline := time.Now().Add(probeTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		info, err := sys.Probe(probeCtx, r.SocketPath)
		cancel()
		if err == nil {
			r.Info = &info
			if r.Config.Transport == config.TransportLocal && (!info.LocalOnly || !info.SSHDisabled) {
				r.fail("probe", fmt.Errorf("daemon at %s did not confirm local-only mode; check the service command, overrides, and binary version", r.SocketPath))
				return
			}
			r.step("probe", fmt.Sprintf("daemon %s answers on %s (%s/%s, %d cpu, kvm=%v, %d slots)",
				info.Version, r.SocketPath, info.Facts.OS, info.Facts.Arch, info.Facts.NumCPU, info.Facts.KVM, info.MaxJobs), false)
			return
		}
		lastErr = err
		time.Sleep(probeInterval)
	}
	r.fail("probe", fmt.Errorf("daemon did not answer on %s within %s: %w", r.SocketPath, probeTimeout, lastErr))
}

func indent(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		lines[i] = "    " + line
	}
	return strings.Join(lines, "\n")
}
