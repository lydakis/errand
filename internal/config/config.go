// Package config loads the two config files: the personal client config
// (peer aliases — never in a repository) and the runner config.
package config

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/lydakis/errand/internal/tailnet"
)

type Peer struct {
	URL           string `toml:"url"`
	SSH           string `toml:"ssh"`
	RemoteCommand string `toml:"remote_command"`
	RemoteSocket  string `toml:"remote_socket"`
}

const SSHScheme = "ssh"
const DisabledListener = "none"

// SSHRemoteCommand returns the errand command to run on an SSH peer.
func (c Client) SSHRemoteCommand(name string) string {
	if name == "" {
		name = c.DefaultPeer
	}
	return c.Peers[name].RemoteCommand
}

// SSHRemoteSocket returns the daemon Unix socket for an SSH peer. An empty
// value tells the remote bridge to use its default runner configuration.
func (c Client) SSHRemoteSocket(name string) string {
	if name == "" {
		name = c.DefaultPeer
	}
	return c.Peers[name].RemoteSocket
}

type Client struct {
	DefaultPeer    string          `toml:"default_peer"`
	ApplyOnSuccess bool            `toml:"apply_on_success"`
	Peers          map[string]Peer `toml:"peers"`
}

func dir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		if !filepath.IsAbs(x) {
			return "", fmt.Errorf("XDG_CONFIG_HOME must be an absolute path")
		}
		return filepath.Join(x, "errand"), nil
	}
	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "errand"), nil
}

func userHomeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating user home directory: %w", err)
	}
	if home == "" || !filepath.IsAbs(home) {
		return "", fmt.Errorf("user home directory must be an absolute path")
	}
	return home, nil
}

// LoadClient reads ~/.config/errand/config.toml; a missing file is an
// empty config, not an error.
func LoadClient() (Client, error) {
	var c Client
	configDir, err := dir()
	if err != nil {
		return c, err
	}
	path := filepath.Join(configDir, "config.toml")
	if _, err := toml.DecodeFile(path, &c); err != nil && !os.IsNotExist(err) {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// PeerURL resolves a peer name (or "" for the default) to a base URL.
func (c Client) PeerURL(name string) (string, error) {
	if name == "" {
		name = c.DefaultPeer
	}
	if name == "" {
		return "", fmt.Errorf("no peer named and no default_peer configured")
	}
	p, ok := c.Peers[name]
	if !ok || (p.URL == "" && p.SSH == "") {
		return "", fmt.Errorf("peer %q is not configured", name)
	}
	if p.URL != "" && p.SSH != "" {
		return "", fmt.Errorf("peer %q sets both url and ssh; choose one transport", name)
	}
	if p.URL != "" && (p.RemoteCommand != "" || p.RemoteSocket != "") {
		return "", fmt.Errorf("peer %q: remote_command and remote_socket require ssh", name)
	}
	if p.SSH != "" {
		if strings.ContainsAny(p.SSH, " \t\r\n:/?#") || strings.HasPrefix(p.SSH, "@") ||
			strings.HasSuffix(p.SSH, "@") || strings.Count(p.SSH, "@") > 1 {
			return "", fmt.Errorf("peer %q: ssh must be an ssh_config host or user@host", name)
		}
		if p.RemoteSocket != "" && !strings.HasPrefix(p.RemoteSocket, "/") {
			return "", fmt.Errorf("peer %q: remote_socket must be an absolute Unix path", name)
		}
		return SSHScheme + "://" + p.SSH, nil
	}
	return strings.TrimSuffix(p.URL, "/"), nil
}

type Daemon struct {
	Listen           string      `toml:"listen"`
	StateDir         string      `toml:"state_dir"`
	AllowUsers       []string    `toml:"allow_users"`
	Capability       string      `toml:"capability"`
	TailscaledSocket string      `toml:"tailscaled_socket"`
	TailscaleCLI     string      `toml:"tailscale_cli"`
	Socket           string      `toml:"socket"`
	Cache            DaemonCache `toml:"cache"`

	// MaxJobs defaults to 1. MaxQueued defaults to 8; zero disables queueing.
	MaxJobs   int `toml:"max_jobs"`
	MaxQueued int `toml:"max_queued"`
}

// DaemonCache uses a 5 GiB, 14-day default when fields are zero.
type DaemonCache struct {
	Disabled bool  `toml:"disabled"`
	MaxBytes int64 `toml:"max_bytes"`
	TTLHours int   `toml:"ttl_hours"`
}

// LoadDaemon reads the runner config (default ~/.config/errand/errandd.toml)
// and fills defaults: listen on the tailnet address, state in ~/.errand.
func LoadDaemon(path string) (Daemon, error) {
	d := Daemon{MaxJobs: 1, MaxQueued: 8}
	explicitPath := path != ""
	if path == "" {
		configDir, err := dir()
		if err != nil {
			return d, err
		}
		path = filepath.Join(configDir, "errandd.toml")
	}
	if _, err := toml.DecodeFile(path, &d); err != nil {
		if explicitPath || !os.IsNotExist(err) {
			return d, fmt.Errorf("%s: %w", path, err)
		}
	}
	if d.Listen == "" {
		d.Listen = "tailnet:7443"
	}
	if d.StateDir == "" {
		home, err := userHomeDir()
		if err != nil {
			return d, err
		}
		d.StateDir = filepath.Join(home, ".errand")
	}
	if d.Cache.TTLHours < 0 || int64(d.Cache.TTLHours) > int64((time.Duration(1<<63-1))/time.Hour) {
		return d, fmt.Errorf("cache ttl_hours must fit in a time.Duration")
	}
	if d.MaxJobs <= 0 {
		return d, fmt.Errorf("max_jobs must be positive")
	}
	if d.MaxQueued < 0 {
		return d, fmt.Errorf("max_queued must not be negative")
	}
	return d, nil
}

// ResolveListen turns "tailnet:PORT" into an address assigned to this node by
// tailscaled, via the discovered identity provider. Explicit listener
// addresses pass through unchanged; loopback runners are refused because
// WhoIs cannot identify them (the Unix socket is the local path).
func ResolveListen(listen string, selfIPs func(context.Context) ([]string, error)) (string, error) {
	if strings.EqualFold(strings.TrimSpace(listen), DisabledListener) {
		return "", nil
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("listen %q: %w", listen, err)
	}
	if strings.EqualFold(host, "localhost") {
		return "", fmt.Errorf("listen %q: loopback runners are not supported", listen)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return "", fmt.Errorf("listen %q: loopback runners are not supported", listen)
	}
	if host != "tailnet" {
		return listen, nil
	}
	if selfIPs == nil {
		return "", fmt.Errorf("listen %q: no tailnet identity provider to resolve the address", listen)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := selfIPs(ctx)
	if err != nil {
		return "", fmt.Errorf("resolving tailnet listener: %w", err)
	}
	ip, err := tailnet.PreferredIP(ips)
	if err != nil {
		return "", fmt.Errorf("resolving tailnet listener: %w", err)
	}
	return net.JoinHostPort(ip, port), nil
}

// SocketPath returns the Unix-socket listener path for a runner config.
func (d Daemon) SocketPath() string {
	if d.Socket != "" {
		return d.Socket
	}
	return filepath.Join(d.StateDir, "errand.sock")
}
