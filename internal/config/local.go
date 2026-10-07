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
	path, err := DaemonPath()
	if err != nil {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return false
	}
	d, err := LoadDaemon(path)
	return err != nil || d.Transport != TransportTailscale
}
