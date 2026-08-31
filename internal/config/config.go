// Package config loads the two config files: the personal client config
// (peer aliases — never in a repository) and the runner config.
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Peer struct {
	URL string `toml:"url"`
}

type Client struct {
	DefaultPeer string          `toml:"default_peer"`
	Peers       map[string]Peer `toml:"peers"`
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
	if !ok || p.URL == "" {
		return "", fmt.Errorf("peer %q is not configured", name)
	}
	return strings.TrimSuffix(p.URL, "/"), nil
}

type Daemon struct {
	Listen           string      `toml:"listen"`
	StateDir         string      `toml:"state_dir"`
	AllowUsers       []string    `toml:"allow_users"`
	Capability       string      `toml:"capability"`
	TailscaledSocket string      `toml:"tailscaled_socket"`
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
// tailscaled. Explicit listener addresses pass through unchanged.
func ResolveListen(listen, tailscaledSocket string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("listen %q: %w", listen, err)
	}
	if host != "tailnet" {
		return listen, nil
	}
	ip, err := tailscaleIP(tailscaledSocket)
	if err != nil {
		return "", fmt.Errorf("resolving tailnet listener: %w", err)
	}
	return net.JoinHostPort(ip, port), nil
}

func tailscaleIP(socket string) (string, error) {
	if socket == "" {
		socket = "/var/run/tailscale/tailscaled.sock"
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socket)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	res, err := client.Get("http://local-tailscaled.sock/localapi/v0/status?peers=false")
	if err != nil {
		return "", fmt.Errorf("reading tailscaled status: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return "", fmt.Errorf("tailscaled status returned %s: %s", res.Status, strings.TrimSpace(string(body)))
	}
	var status struct {
		BackendState string   `json:"BackendState"`
		TailscaleIPs []string `json:"TailscaleIPs"`
	}
	if err := json.NewDecoder(res.Body).Decode(&status); err != nil {
		return "", fmt.Errorf("decoding tailscaled status: %w", err)
	}
	var ipv6 string
	for _, raw := range status.TailscaleIPs {
		ip := net.ParseIP(raw)
		if ip == nil {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			return v4.String(), nil
		}
		if ipv6 == "" {
			ipv6 = ip.String()
		}
	}
	if ipv6 != "" {
		return ipv6, nil
	}
	return "", fmt.Errorf("tailscaled has no assigned IP (state %s); is Tailscale up?", status.BackendState)
}
