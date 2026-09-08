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
	installed := c.DefaultPeer == "local"
	if path, err := DaemonPath(); err == nil {
		if _, err := os.Stat(path); err == nil {
			d, err := LoadDaemon(path)
			// Tailscale-only sockets expose health/setup but refuse job APIs.
			// Do not add an unusable target to every unqualified ps invocation.
			installed = installed || err != nil || d.Transport != TransportTailscale
		}
	}
	if installed {
		c.Peers = maps.Clone(c.Peers)
		if c.Peers == nil {
			c.Peers = map[string]Peer{}
		}
		c.Peers["local"] = Peer{}
	}
	return c
}
