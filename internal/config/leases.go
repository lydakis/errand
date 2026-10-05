package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/lydakis/errand/internal/proto"
)

// LeasePeer is the peer entry a lease target stands for.
func LeasePeer(t proto.LeaseTarget) Peer {
	return Peer{URL: t.URL, SSH: t.SSH, RemoteCommand: t.RemoteCommand, RemoteSocket: t.RemoteSocket}
}

// StateDirectory holds client state that is not configuration.
func StateDirectory() (string, error) {
	if stateHome := os.Getenv("XDG_STATE_HOME"); stateHome != "" {
		if !filepath.IsAbs(stateHome) {
			return "", fmt.Errorf("XDG_STATE_HOME must be an absolute path")
		}
		return filepath.Join(stateHome, "errand"), nil
	}
	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "errand"), nil
}

// FindLeasePeer, when set, resolves a name no peer is configured under as a
// machine the caller leased from a cloud peer, such as cloud-7f3a. It
// reports false for a name that is not one. Clients keep no record of their
// leases: a process asks the cloud peer the first time it uses the name.
var FindLeasePeer func(c Client, name string) (Peer, bool, error)

var leasePeers sync.Map // name → Peer, as FindLeasePeer found it in this process

// peer looks up a configured peer, or else a lease peer.
func (c Client) peer(name string) (Peer, bool, error) {
	if p, ok := c.Peers[name]; ok {
		return p, true, nil
	}
	if p, ok := leasePeers.Load(name); ok {
		return p.(Peer), true, nil
	}
	if FindLeasePeer == nil || name == "" {
		return Peer{}, false, nil
	}
	p, ok, err := FindLeasePeer(c, name)
	if err != nil || !ok {
		return Peer{}, false, err
	}
	leasePeers.Store(name, p)
	return p, true, nil
}
