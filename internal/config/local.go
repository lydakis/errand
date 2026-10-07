package config

import (
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
)

// LocalURL encodes the full socket path in the authority so ordinary API paths
// and durable job state retain an unambiguous, configuration-independent target.
func LocalURL(socket string) (string, error) {
	if !filepath.IsAbs(socket) || strings.ContainsRune(socket, 0) {
		return "", fmt.Errorf("local socket must be an absolute Unix path")
	}
	return "unix://" + hex.EncodeToString([]byte(filepath.Clean(socket))), nil
}

// WithLocalPeer includes an explicitly selected default or an installed local
// runner in inventory. It never selects local execution as a fallback.
func (c Client) WithLocalPeer() Client {
	if _, exists := c.Peers["local"]; exists {
		return c
	}
	if c.DefaultPeer == "local" || localRunnerInstalled() {
		c.Peers = maps.Clone(c.Peers)
		if c.Peers == nil {
			c.Peers = map[string]Peer{}
		}
		c.Peers["local"] = Peer{}
	}
	return c
}

// localRunnerInstalled reports whether this machine has a runner config whose
// socket takes jobs. Tailscale-only sockets expose health/setup but refuse job
// APIs, so they are not offered as a target.
func localRunnerInstalled() bool {
	_, ok := installedLocalRunner()
	return ok
}

// installedLocalRunner is localRunnerInstalled with the runner's config, which
// is empty when the file exists but cannot be loaded.
func installedLocalRunner() (Daemon, bool) {
	path, err := DaemonPath()
	if err != nil {
		return Daemon{}, false
	}
	if _, err := os.Stat(path); err != nil {
		return Daemon{}, false
	}
	d, err := LoadDaemon(path)
	if err != nil {
		return Daemon{}, true
	}
	return d, d.Transport != TransportTailscale
}
