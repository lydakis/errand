package setup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/tailnet"
)

// DiagnosticSystem exposes only observations. In particular, diagnosis cannot
// use setup's write, writable-directory probe, or restart-lease operations.
type DiagnosticSystem interface {
	serviceSystem
	Home() (string, error)
	DaemonPath() (string, error)
	Lstat(string) (os.FileInfo, error)
	Username() string
	Executable() (string, error)
	LookPath(string) (string, error)
	Stat(string) (os.FileInfo, error)
	LoadDaemon(string) (config.Daemon, error)
	Discover(string, string) (tailnet.Provider, error)
	Probe(context.Context, string) (proto.Info, error)
}

func (RealSystem) LookPath(name string) (string, error)          { return exec.LookPath(name) }
func (RealSystem) Stat(path string) (os.FileInfo, error)         { return os.Stat(path) }
func (RealSystem) Lstat(path string) (os.FileInfo, error)        { return os.Lstat(path) }
func (RealSystem) DaemonPath() (string, error)                   { return config.DaemonPath() }
func (RealSystem) LoadDaemon(path string) (config.Daemon, error) { return config.LoadDaemon(path) }

type DiagnosticCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

type Diagnosis struct {
	Configured bool
	Checks     []DiagnosticCheck
	Info       *proto.Info
	SocketPath string
}

func (r Diagnosis) OK() bool {
	for _, c := range r.Checks {
		if c.Status == "error" {
			return false
		}
	}
	return !r.Configured || r.Info != nil
}

func (r *Diagnosis) add(name, status, detail, hint string) {
	r.Checks = append(r.Checks, DiagnosticCheck{name, status, detail, hint})
}

// Diagnose inspects this user's local runner. Each external observation has
// a deadline, and independent checks continue after failures. Service status
// describes setup's named user service; the socket probe proves responsiveness.
func Diagnose(ctx context.Context, configPath string, sys DiagnosticSystem) Diagnosis {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r := Diagnosis{}
	diagnoseBinary(sys, &r)
	d, configErr := sys.LoadDaemon(configPath)
	serviceCtx, stopService := context.WithTimeout(ctx, 4*time.Second)
	active, serviceErr := serviceActive(serviceCtx, sys)
	stopService()
	definition, definitionErr := diagnosticServiceDefinition(sys)
	expectService := active || definition
	defaultPath, pathErr := configPath, error(nil)
	if configPath == "" {
		defaultPath, pathErr = sys.DaemonPath()
	}
	// Missing evidence means this may be a client-only installation. Inaccessible
	// or dangling files count as evidence so failures cannot turn into a skip.
	r.Configured = configPath != "" || configErr != nil || pathErr != nil || definitionErr != nil || expectService || diagnosticPathPresent(sys, defaultPath)
	if configErr == nil {
		r.Configured = r.Configured || diagnosticPathPresent(sys, d.SocketPath())
	}
	if !r.Configured {
		detail := "Local runner not configured; no saved runner config, service definition, loaded service, or socket was found."
		if serviceErr != nil {
			detail = "Local runner not configured in saved files; no socket was found and service-manager status was unavailable."
		}
		r.add("runner", "skipped", detail, "")
		return r
	}
	if pathErr != nil {
		r.add("configuration", "error", pathErr.Error(), "Check the runner configuration location.")
	}
	if definitionErr != nil {
		r.add("service", "error", definitionErr.Error(), "Check the service user's home directory.")
	}
	if configErr != nil {
		r.add("configuration", "error", configErr.Error(), "Use the same --config file as the runner service and correct the reported setting.")
	} else {
		r.SocketPath = d.SocketPath()
		r.add("configuration", "ok", "Loaded runner settings; local socket: "+r.SocketPath, "")
	}
	diagnoseSSHPath(sys, &r)
	diagnoseService(ctx, sys, &r, expectService, active, serviceErr)
	if configErr != nil {
		r.add("socket", "skipped", "Runner configuration did not resolve.", "")
		r.add("runner", "skipped", "No socket probe was made.", "")
		return r
	}
	diagnoseTailnet(ctx, sys, d, &r)
	if !filepath.IsAbs(r.SocketPath) {
		r.add("socket", "error", "The configured socket path is relative to the service's working directory.", "Use an absolute socket or state_dir path in the runner configuration.")
		r.add("runner", "skipped", "No probe was made because the configured socket path is ambiguous.", "")
		return r
	}
	info, err := sys.Stat(r.SocketPath)
	switch {
	case err != nil:
		r.add("socket", "error", fmt.Sprintf("Cannot inspect %s: %v", r.SocketPath, err), "Check the socket path, parent directory access, and whether the runner is started. Run doctor as the service's user.")
	case info.Mode()&os.ModeSocket == 0:
		r.add("socket", "error", r.SocketPath+" is not a Unix socket.", "Correct the runner's socket path; inspect the existing file before replacing anything.")
	default:
		if info.Mode().Perm() != 0o600 {
			r.add("socket", "error", fmt.Sprintf("%s has mode %04o; Errand creates private sockets with mode 0600.", r.SocketPath, info.Mode().Perm()), "Inspect socket ownership and restore private permissions as the service's user.")
		} else {
			r.add("socket", "ok", r.SocketPath+" is a private Unix socket (0600).", "")
		}
		probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		remote, err := sys.Probe(probeCtx, r.SocketPath)
		if err != nil {
			r.add("runner", "error", err.Error(), "Check service logs, socket ownership, and the service's --config path. A stale socket file does not establish that a runner is alive.")
		} else if remote.Version == "" || remote.Proto != proto.ProtoVersion {
			r.add("runner", "error", "The socket did not return compatible Errand info.", "Use compatible client and runner versions and verify the socket belongs to Errand.")
		} else {
			r.Info = &remote
			status, hint := "ok", ""
			if remote.Busy {
				status, hint = "warning", "The runner is busy; later submissions may queue or be refused."
			}
			r.add("runner", status, fmt.Sprintf("Runner %s answers over its local socket (%s/%s, %d slots).", remote.Version, remote.Facts.OS, remote.Facts.Arch, remote.MaxJobs), hint)
		}
		return r
	}
	r.add("runner", "skipped", "No probe was made because the configured Unix socket is unavailable.", "")
	return r
}

func diagnoseBinary(sys DiagnosticSystem, r *Diagnosis) {
	exe, err := sys.Executable()
	if err != nil {
		r.add("binary", "error", err.Error(), "Check the installed Errand executable.")
		return
	}
	info, err := sys.Stat(exe)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		r.add("binary", "error", "The running binary's installation path is missing or not executable: "+exe, "Check installation paths and symlinks before restarting the service.")
	} else {
		r.add("binary", "ok", "Running executable: "+exe, "")
	}
	resolved, err := sys.LookPath("errand")
	if err != nil {
		r.add("path", "warning", "errand does not resolve on this shell's PATH.", "Add its installation directory to PATH, or configure an absolute remote_command for SSH peers.")
	} else if resolved != exe {
		r.add("path", "warning", "PATH resolves errand to "+resolved+"; this invocation uses "+exe, "Check for stale installations. Symlinks may refer to the same binary; compare versions if unsure.")
	} else {
		r.add("path", "ok", "PATH resolves this executable.", "")
	}
}

func diagnoseSSHPath(sys DiagnosticSystem, r *Diagnosis) {
	sshPath := filepath.Join(pathSymlinkDir, "errand")
	if info, err := sys.Stat(sshPath); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		r.add("ssh-path", "warning", "No executable at "+sshPath+", the path used by setup for SSH callers.", "Non-interactive SSH shells may have a different PATH. Configure the peer's remote_command with an absolute executable path if needed.")
	} else {
		r.add("ssh-path", "ok", "An executable is available at "+sshPath+"; actual SSH shell resolution is checked by doctor on the client.", "")
	}
}

func diagnoseService(ctx context.Context, sys DiagnosticSystem, r *Diagnosis, expected, active bool, err error) {
	if sys.GOOS() != "linux" && sys.GOOS() != "darwin" {
		r.add("service", "skipped", "No setup service-manager integration for "+sys.GOOS()+".", "Inspect the service manager used to launch this runner.")
		return
	}
	status := "warning"
	if expected {
		status = "error"
	}
	// Service-manager output may contain the service's complete environment.
	// Never copy its output or wrapped command error into diagnostic reports.
	switch {
	case err != nil:
		r.add("service", status, "Could not query the setup-managed user service.", "Check systemctl --user or launchctl in the service user's session. Custom service managers must be inspected separately.")
	case !active:
		r.add("service", status, "The setup-managed user service is not loaded or active.", "Inspect your service manager and logs. An installed service is expected to be active even if a separate manually started runner answers.")
	default:
		r.add("service", "ok", "The setup-managed user service is loaded or active; the socket check establishes responsiveness.", "")
	}
	if sys.GOOS() == "linux" {
		lingerCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		out, err := sys.Run(lingerCtx, "loginctl", "show-user", sys.Username(), "-p", "Linger")
		cancel()
		if err != nil || strings.TrimSpace(out) != "Linger=yes" {
			r.add("linger", "warning", "User-service persistence after logout is not confirmed.", "Check loginctl show-user for the runner's user; setup can enable linger when configuring a user service.")
		} else {
			r.add("linger", "ok", "Linger is enabled for the service user.", "")
		}
	}
}

func diagnosticPathPresent(sys DiagnosticSystem, path string) bool {
	if path == "" {
		return false
	}
	_, err := sys.Lstat(path)
	return !os.IsNotExist(err)
}

func diagnosticServiceDefinition(sys DiagnosticSystem) (bool, error) {
	if sys.GOOS() != "linux" && sys.GOOS() != "darwin" {
		return false, nil
	}
	home, err := sys.Home()
	if err != nil {
		return false, err
	}
	path := filepath.Join(home, linuxUnitSubdir, DefaultServiceName+".service")
	if sys.GOOS() == "darwin" {
		path = filepath.Join(home, darwinAgentSubdir, LaunchAgentLabel+".plist")
	}
	return diagnosticPathPresent(sys, path), nil
}

func diagnoseTailnet(ctx context.Context, sys DiagnosticSystem, d config.Daemon, r *Diagnosis) {
	if strings.EqualFold(strings.TrimSpace(d.Listen), config.DisabledListener) {
		r.add("tailnet", "skipped", "TCP listening is disabled; this runner uses its local socket and SSH.", "")
		return
	}
	provider, err := sys.Discover(d.TailscaledSocket, d.TailscaleCLI)
	if err == nil {
		selfCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		var self tailnet.Self
		self, err = provider.Self(selfCtx)
		if err == nil && !tailnet.SupportsDestinationScopedWhoIs(self.Version) {
			err = fmt.Errorf("tailscaled %q is too old; requires 1.100 or newer", self.Version)
		}
		if err == nil {
			_, err = config.ResolveListen(d.Listen, func(context.Context) ([]string, error) { return self.IPs, nil })
		}
	}
	if err != nil {
		r.add("tailnet", "error", err.Error(), "Check Tailscale and the saved tailscaled_socket or tailscale_cli settings.")
	} else {
		r.add("tailnet", "ok", "The configured identity provider answers and the listen address resolves.", "")
	}
}
