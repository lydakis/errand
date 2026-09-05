// Package config loads personal and runner configuration and resolves run settings.
package config

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/lydakis/errand/internal/tailnet"
	"github.com/lydakis/errand/internal/workspace"
)

type Peer struct {
	URL           string `toml:"url"`
	SSH           string `toml:"ssh,omitempty"`
	RemoteCommand string `toml:"remote_command,omitempty"`
	RemoteSocket  string `toml:"remote_socket,omitempty"`
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
	Artifacts      workspace.Artifacts          `toml:"artifacts"`
	Session        workspace.Session            `toml:"session"`
	Environment    workspace.Environment        `toml:"env,omitempty"`
	Profiles       map[string]workspace.Profile `toml:"profiles,omitempty"`
	DefaultPeer    string                       `toml:"default_peer,omitempty"`
	ApplyOnSuccess *bool                        `toml:"apply_on_success,omitempty"`
	Peers          map[string]Peer              `toml:"peers,omitempty"`
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
		if p.RemoteCommand != "" && !strings.HasPrefix(p.RemoteCommand, "/") {
			return "", fmt.Errorf("peer %q: remote_command must be an absolute executable path", name)
		}
		return SSHScheme + "://" + p.SSH, nil
	}
	u, err := url.Parse(p.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("peer %q: url must be an http:// or https:// runner base URL", name)
	}
	return strings.TrimSuffix(p.URL, "/"), nil
}

// ValidatePeer checks that a peer can be resolved by ordinary client commands.
func ValidatePeer(name string, peer Peer) error {
	if err := validatePeerName(name); err != nil {
		return err
	}
	_, err := (Client{Peers: map[string]Peer{name: peer}}).PeerURL(name)
	return err
}

type Daemon struct {
	Listen           string      `toml:"listen"`
	StateDir         string      `toml:"state_dir"`
	AllowUsers       []string    `toml:"allow_users"`
	DenyUsers        []string    `toml:"deny_users"`
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
		var err error
		path, err = DaemonPath()
		if err != nil {
			return d, err
		}
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

// DaemonPath is the default runner config path, including XDG overrides.
func DaemonPath() (string, error) {
	configDir, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "errandd.toml"), nil
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

// ClientPath is the personal client config file.
func ClientPath() (string, error) {
	configDir, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "config.toml"), nil
}

// AddPeer records a peer, making the first peer the default. New peers are
// appended so existing comments and ordering survive; replacements re-encode.
func AddPeer(path, name string, peer Peer, replace bool) (madeDefault bool, err error) {
	state, err := prepareAddPeer(path, name, peer, replace)
	if err != nil {
		return false, err
	}
	c := state.client
	madeDefault = state.plan.MadeDefault
	if c.Peers == nil {
		c.Peers = map[string]Peer{}
	}
	c.Peers[name] = peer
	if madeDefault {
		c.DefaultPeer = name
	}
	if state.plan.Replacing || !state.existing {
		return madeDefault, writeClient(path, c)
	}
	if madeDefault && state.defaultDefined {
		return madeDefault, writeClient(path, c)
	}
	var block strings.Builder
	block.WriteString(fmt.Sprintf("\n[peers.%s]\n", name))
	if peer.URL != "" {
		block.WriteString(fmt.Sprintf("url = %q\n", peer.URL))
	} else {
		block.WriteString(fmt.Sprintf("ssh = %q\n", peer.SSH))
		if peer.RemoteCommand != "" {
			block.WriteString(fmt.Sprintf("remote_command = %q\n", peer.RemoteCommand))
		}
		if peer.RemoteSocket != "" {
			block.WriteString(fmt.Sprintf("remote_socket = %q\n", peer.RemoteSocket))
		}
	}
	text := state.raw
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if madeDefault {
		text = fmt.Sprintf("default_peer = %q\n", name) + text
	}
	return madeDefault, writeFile(path, text+block.String())
}

// AddPeerPlan describes the observable effect of AddPeer without writing.
type AddPeerPlan struct {
	MadeDefault bool
	Replacing   bool
}

// PlanAddPeer validates an add against the current client configuration.
func PlanAddPeer(path, name string, peer Peer, replace bool) (AddPeerPlan, error) {
	state, err := prepareAddPeer(path, name, peer, replace)
	return state.plan, err
}

type addPeerState struct {
	client         Client
	raw            string
	existing       bool
	defaultDefined bool
	plan           AddPeerPlan
}

func prepareAddPeer(path, name string, peer Peer, replace bool) (addPeerState, error) {
	var state addPeerState
	if err := ValidatePeer(name, peer); err != nil {
		return state, err
	}
	if raw, readErr := os.ReadFile(path); readErr == nil {
		state.existing = true
		state.raw = string(raw)
		metadata, err := toml.Decode(state.raw, &state.client)
		if err != nil {
			return state, fmt.Errorf("%s: %w", path, err)
		}
		state.defaultDefined = metadata.IsDefined("default_peer")
	} else if !os.IsNotExist(readErr) {
		return state, readErr
	}
	_, state.plan.Replacing = state.client.Peers[name]
	if state.plan.Replacing && !replace {
		return state, fmt.Errorf("peer %q already exists in %s (use --force to replace it)", name, path)
	}
	state.plan.MadeDefault = state.client.DefaultPeer == "" && len(state.client.Peers) == 0
	return state, nil
}

// RemovePeer deletes a peer, clearing default_peer if it pointed there.
func RemovePeer(path, name string) (clearedDefault bool, err error) {
	var c Client
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if _, err := toml.Decode(string(raw), &c); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if _, ok := c.Peers[name]; !ok {
		return false, fmt.Errorf("peer %q is not configured in %s", name, path)
	}
	delete(c.Peers, name)
	if c.DefaultPeer == name {
		c.DefaultPeer = ""
		clearedDefault = true
	}
	return clearedDefault, writeClient(path, c)
}

func validatePeerName(name string) error {
	if name == "" {
		return fmt.Errorf("peer name must not be empty")
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return fmt.Errorf("peer name %q may contain only letters, digits, '-' and '_'", name)
		}
	}
	return nil
}

func writeClient(path string, c Client) error {
	var b strings.Builder
	b.WriteString("# errand client configuration — peers are personal aliases, never repository state.\n")
	if err := toml.NewEncoder(&b).Encode(c); err != nil {
		return err
	}
	return writeFile(path, b.String())
}

func writeFile(path, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
