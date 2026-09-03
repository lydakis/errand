package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

// Identity is who a request came from and what they may do. Fail closed:
// no identity or no matching authorization means the request is refused.
type Identity struct {
	Login   string
	UserID  int64
	Node    string
	NodeID  string
	Method  string // capability | allowlist | insecure-test
	Actions map[string]bool
}

func (id Identity) Allowed(action string) bool {
	return id.Actions["*"] || id.Actions[action]
}

// Owner is the ownership principal per the design: the authenticated
// tailnet user when one exists, otherwise the node identity.
func (id Identity) Owner() string {
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

type whoisResponse struct {
	Node struct {
		Name     string `json:"Name"`
		StableID string `json:"StableID"`
	} `json:"Node"`
	UserProfile struct {
		ID        int64  `json:"ID"`
		LoginName string `json:"LoginName"`
	} `json:"UserProfile"`
	CapMap map[string][]json.RawMessage `json:"CapMap"`
}

type capRule struct {
	Actions []string `json:"actions"`
}

// whois asks the local tailscaled who owns remoteAddr, over its Unix
// socket LocalAPI.
func newWhoisClient(socket string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socket)
			},
		},
	}
}

func (d *Daemon) whois(remoteAddr string, localAddr net.Addr) (whoisResponse, error) {
	var resp whoisResponse
	if d.whoisClient == nil {
		return resp, fmt.Errorf("whois: client is not initialized")
	}
	destination, err := acceptedDestinationIP(localAddr)
	if err != nil {
		return resp, fmt.Errorf("whois: %w", err)
	}
	query := url.Values{"addr": {remoteAddr}, "dst_ip": {destination}}
	u := "http://local-tailscaled.sock/localapi/v0/whois?" + query.Encode()
	res, err := d.whoisClient.Get(u)
	if err != nil {
		return resp, fmt.Errorf("whois: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return resp, fmt.Errorf("whois: tailscaled returned %s", res.Status)
	}
	if !supportsDestinationScopedWhois(res.Header.Get("Tailscale-Version")) {
		return resp, fmt.Errorf("whois: tailscaled %q does not support destination-scoped WhoIs (requires 1.100 or newer)", res.Header.Get("Tailscale-Version"))
	}
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return resp, fmt.Errorf("whois: %w", err)
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		return resp, fmt.Errorf("whois: draining response: %w", err)
	}
	return resp, nil
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

func supportsDestinationScopedWhois(raw string) bool {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(raw), "v"), ".")
	if len(parts) < 2 {
		return false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	return major > 1 || major == 1 && minor >= 100
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
		Login: w.UserProfile.LoginName, UserID: w.UserProfile.ID,
		Node: w.Node.Name, NodeID: w.Node.StableID, Actions: map[string]bool{},
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

func admissionOwner(a proto.Admission) string {
	return (Identity{
		Login: a.UserLogin, UserID: a.UserID,
		Node: a.NodeName, NodeID: a.NodeID,
	}).Owner()
}
