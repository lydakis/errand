package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/tailnet"
)

// Identity is who a request came from and what they may do. Fail closed:
// no identity or no matching authorization means the request is refused.
type Identity struct {
	Login   string
	UserID  int64
	Node    string
	NodeID  string
	Method  string // capability | allowlist | local | insecure-test
	Actions map[string]bool

	// Local is set for Unix-socket callers (the SSH transport terminates
	// there); identity comes from kernel peer credentials, not WhoIs.
	Local     bool
	LocalUID  uint32
	LocalUser string
}

func (id Identity) Allowed(action string) bool {
	return id.Actions["*"] || id.Actions[action]
}

// Owner is the ownership principal per the design: the authenticated
// tailnet user when one exists, otherwise the node identity.
func (id Identity) Owner() string {
	if id.Local {
		return fmt.Sprintf("local-user:%d", id.LocalUID)
	}
	if id.Login != "" {
		if id.UserID == 0 {
			return ""
		}
		return fmt.Sprintf("tailnet-user:%d", id.UserID)
	}
	if id.NodeID == "" {
		return ""
	}
	return "tailnet-node:" + id.NodeID
}

type capRule struct {
	Actions []string `json:"actions"`
}

// whois asks tailscaled (through whichever provider was discovered) who owns
// remoteAddr as seen arriving at this listener's address.
func (d *Daemon) whois(remoteAddr string, localAddr net.Addr) (tailnet.WhoIs, error) {
	if d.identity == nil {
		return tailnet.WhoIs{}, fmt.Errorf("whois: no tailnet identity provider is configured")
	}
	destination, err := acceptedDestinationIP(localAddr)
	if err != nil {
		return tailnet.WhoIs{}, fmt.Errorf("whois: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return d.identity.WhoIs(ctx, remoteAddr, destination)
}

func acceptedDestinationIP(localAddr net.Addr) (string, error) {
	if localAddr == nil {
		return "", fmt.Errorf("connection has no local destination address")
	}
	host, _, err := net.SplitHostPort(localAddr.String())
	if err != nil {
		return "", fmt.Errorf("invalid local destination %q: %w", localAddr.String(), err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" {
		return "", fmt.Errorf("local destination %q is not a plain IP address", host)
	}
	ip = ip.Unmap()
	if ip.IsUnspecified() {
		return "", fmt.Errorf("local destination %q is unspecified", host)
	}
	return ip.String(), nil
}

func sameHostConnection(remoteAddr string, localAddr net.Addr) bool {
	remoteHost, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	remoteIP, err := netip.ParseAddr(remoteHost)
	if err != nil || remoteIP.Zone() != "" {
		return false
	}
	destination, err := acceptedDestinationIP(localAddr)
	if err != nil {
		return false
	}
	return remoteIP.Unmap().String() == destination
}

func unsupportedSelfTarget(remoteAddr string, localAddr net.Addr) bool {
	if !sameHostConnection(remoteAddr, localAddr) {
		return false
	}
	destination, err := acceptedDestinationIP(localAddr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(destination)
	return err == nil && !ip.IsLoopback()
}

// identify resolves and authorizes a caller. Authorization comes from the
// tailnet ACL capability (action schema, additive merge) or the runner's
// local allowlist; anything else is refused.
func (d *Daemon) identify(remoteAddr string, localAddr net.Addr) (Identity, error) {
	if d.cfg.InsecureNoAuth {
		return Identity{Login: "test@insecure", Method: "insecure-test", Actions: map[string]bool{"*": true}}, nil
	}
	w, err := d.whois(remoteAddr, localAddr)
	if err != nil {
		return Identity{}, err
	}
	id := Identity{
		Login: w.LoginName, UserID: w.UserID,
		Node: w.NodeName, NodeID: w.NodeStableID, Actions: map[string]bool{},
	}
	if id.Owner() == "" {
		return id, fmt.Errorf("whois returned no stable owner identity for %s (%s)", id.Login, id.Node)
	}
	if rules, ok := w.CapMap[d.cfg.Capability]; ok {
		id.Method = "capability"
		for _, raw := range rules {
			var r capRule
			if err := json.Unmarshal(raw, &r); err != nil {
				continue // malformed grant values grant nothing
			}
			for _, a := range r.Actions {
				id.Actions[a] = true
			}
		}
	}
	for _, u := range d.cfg.AllowUsers {
		if u != "" && u == id.Login {
			if id.Method == "" {
				id.Method = "allowlist"
			}
			id.Actions["*"] = true
		}
	}
	if len(id.Actions) == 0 {
		return id, fmt.Errorf("caller %s (%s) holds no errand authorization", id.Login, id.Node)
	}
	return id, nil
}

// identifyRequest resolves a request's caller: Unix-socket peers by kernel
// credentials, everyone else by tailnet WhoIs.
func (d *Daemon) identifyRequest(r *http.Request) (Identity, error) {
	if peer, ok := localPeerFromContext(r.Context()); ok {
		return d.identifyLocal(peer)
	}
	localAddr, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	return d.identify(r.RemoteAddr, localAddr)
}

func (d *Daemon) identifyLocal(peer LocalPeer) (Identity, error) {
	id := Identity{
		Method: "local", Local: true, LocalUID: peer.UID, LocalUser: peer.User,
		Login: peer.User, Actions: map[string]bool{},
	}
	if d.cfg.InsecureNoAuth {
		id.Actions["*"] = true
		return id, nil
	}
	if peer.UID != d.selfUID {
		return id, fmt.Errorf("local user %s (uid %d) holds no errand authorization", peer.User, peer.UID)
	}
	id.Actions["*"] = true
	return id, nil
}

func admissionOwner(a proto.Admission) string {
	if a.LocalUser != "" || a.Method == "local" {
		return fmt.Sprintf("local-user:%d", a.LocalUID)
	}
	return (Identity{
		Login: a.UserLogin, UserID: a.UserID,
		Node: a.NodeName, NodeID: a.NodeID,
	}).Owner()
}
