// Package tailnet resolves Tailscale identity through LocalAPI or the CLI.
package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// WhoIs is the subset of a Tailscale WhoIs answer errand relies on.
type WhoIs struct {
	NodeName     string
	NodeStableID string
	UserID       int64
	LoginName    string
	// CapMap is destination-scoped when the provider supports it. Providers
	// that cannot scope capabilities return nil so callers never grant from
	// an over-broad map.
	CapMap map[string][]json.RawMessage
}

// Self describes this node as tailscaled sees it.
type Self struct {
	Login    string
	UserID   int64
	DNSName  string
	HostName string
	IPs      []string
	OS       string
	Version  string
}

// Provider is one way of reaching tailscaled.
type Provider interface {
	Name() string
	WhoIs(ctx context.Context, remoteAddr, dstIP string) (WhoIs, error)
	SelfIPs(ctx context.Context) ([]string, error)
	Self(ctx context.Context) (Self, error)
}

// statusWire is the subset of tailscaled's status document both providers
// decode (LocalAPI /status and `tailscale status --json` share it).
type statusWire struct {
	Version      string `json:"Version"`
	BackendState string `json:"BackendState"`
	Self         struct {
		DNSName      string   `json:"DNSName"`
		HostName     string   `json:"HostName"`
		UserID       int64    `json:"UserID"`
		TailscaleIPs []string `json:"TailscaleIPs"`
		OS           string   `json:"OS"`
	} `json:"Self"`
	User map[string]struct {
		LoginName string `json:"LoginName"`
	} `json:"User"`
}

func (w statusWire) toSelf() (Self, error) {
	self := Self{
		UserID:   w.Self.UserID,
		DNSName:  strings.TrimSuffix(w.Self.DNSName, "."),
		HostName: w.Self.HostName,
		IPs:      w.Self.TailscaleIPs,
		OS:       w.Self.OS,
		Version:  strings.TrimSpace(w.Version),
	}
	if u, ok := w.User[strconv.FormatInt(w.Self.UserID, 10)]; ok {
		self.Login = u.LoginName
	}
	if self.Login == "" {
		return self, fmt.Errorf("tailscaled status (state %q) names no login for this node; is Tailscale logged in?", w.BackendState)
	}
	return self, nil
}

// SupportsDestinationScopedWhoIs reports whether a tailscaled version honors
// dst_ip on WhoIs (1.100+), as capability-based authorization requires.
func SupportsDestinationScopedWhoIs(raw string) bool {
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

func defaultSocketCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"/var/run/tailscaled.socket", "/var/run/tailscale/tailscaled.sock"}
	default:
		return []string{"/var/run/tailscale/tailscaled.sock"}
	}
}

func Discover(socket, cli string) (Provider, error) {
	if socket != "" {
		if err := socketUsable(socket); err != nil {
			return nil, fmt.Errorf("tailscaled socket %q: %w", socket, err)
		}
		return NewLocalAPI(socket), nil
	}
	if cli != "" {
		path, err := exec.LookPath(cli)
		if err != nil {
			return nil, fmt.Errorf("tailscale cli %q: %w", cli, err)
		}
		return NewCLI(path), nil
	}
	return discoverDefault(defaultSocketCandidates())
}

func discoverDefault(candidates []string) (Provider, error) {
	var tried []string
	for _, candidate := range candidates {
		if socketUsable(candidate) == nil {
			return NewLocalAPI(candidate), nil
		}
		tried = append(tried, "socket "+candidate)
	}
	if path, err := exec.LookPath("tailscale"); err == nil {
		return NewCLI(path), nil
	}
	tried = append(tried, "tailscale CLI on PATH")
	return nil, fmt.Errorf("no way to reach tailscaled (tried: %s); set tailscaled_socket or tailscale_cli in errandd.toml",
		strings.Join(tried, ", "))
}

func socketUsable(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return errors.New("not a unix socket")
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

type localAPI struct {
	socket string
	client *http.Client
}

func NewLocalAPI(socket string) Provider {
	return &localAPI{
		socket: socket,
		client: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var dialer net.Dialer
					return dialer.DialContext(ctx, "unix", socket)
				},
			},
		},
	}
}

func (p *localAPI) Name() string { return "localapi:" + p.socket }

type whoisWire struct {
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

func (w whoisWire) toWhoIs() WhoIs {
	return WhoIs{
		NodeName: w.Node.Name, NodeStableID: w.Node.StableID,
		UserID: w.UserProfile.ID, LoginName: w.UserProfile.LoginName, CapMap: w.CapMap,
	}
}

func (p *localAPI) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://local-tailscaled.sock"+path, nil)
	if err != nil {
		return nil, err
	}
	res, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		res.Body.Close()
		return nil, fmt.Errorf("tailscaled returned %s: %s", res.Status, strings.TrimSpace(string(body)))
	}
	return res, nil
}

func (p *localAPI) WhoIs(ctx context.Context, remoteAddr, dstIP string) (WhoIs, error) {
	query := url.Values{"addr": {remoteAddr}, "dst_ip": {dstIP}}
	res, err := p.get(ctx, "/localapi/v0/whois?"+query.Encode())
	if err != nil {
		return WhoIs{}, fmt.Errorf("whois: %w", err)
	}
	defer res.Body.Close()
	if v := res.Header.Get("Tailscale-Version"); !SupportsDestinationScopedWhoIs(v) {
		return WhoIs{}, fmt.Errorf("whois: tailscaled %q does not support destination-scoped WhoIs (requires 1.100 or newer)", v)
	}
	var wire whoisWire
	if err := json.NewDecoder(res.Body).Decode(&wire); err != nil {
		return WhoIs{}, fmt.Errorf("whois: %w", err)
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		return WhoIs{}, fmt.Errorf("whois: draining response: %w", err)
	}
	return wire.toWhoIs(), nil
}

func (p *localAPI) Self(ctx context.Context) (Self, error) {
	res, err := p.get(ctx, "/localapi/v0/status?peers=false")
	if err != nil {
		return Self{}, err
	}
	defer res.Body.Close()
	var wire statusWire
	if err := json.NewDecoder(res.Body).Decode(&wire); err != nil {
		return Self{}, fmt.Errorf("decoding tailscaled status: %w", err)
	}
	if wire.Version == "" {
		wire.Version = res.Header.Get("Tailscale-Version")
	}
	return wire.toSelf()
}

func (p *localAPI) SelfIPs(ctx context.Context) ([]string, error) {
	res, err := p.get(ctx, "/localapi/v0/status?peers=false")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var status struct {
		BackendState string   `json:"BackendState"`
		TailscaleIPs []string `json:"TailscaleIPs"`
	}
	if err := json.NewDecoder(res.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decoding tailscaled status: %w", err)
	}
	if len(status.TailscaleIPs) == 0 {
		return nil, fmt.Errorf("tailscaled has no assigned IP (state %s); is Tailscale up?", status.BackendState)
	}
	return status.TailscaleIPs, nil
}

type cli struct {
	path string
}

func NewCLI(path string) Provider { return &cli{path: path} }

func (p *cli) Name() string { return "cli:" + p.path }

func (p *cli) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.path, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", p.path, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (p *cli) WhoIs(ctx context.Context, remoteAddr, _ string) (WhoIs, error) {
	out, err := p.run(ctx, "whois", "--json", remoteAddr)
	if err != nil {
		return WhoIs{}, fmt.Errorf("whois: %w", err)
	}
	var wire whoisWire
	if err := json.Unmarshal(out, &wire); err != nil {
		return WhoIs{}, fmt.Errorf("whois: decoding cli output: %w", err)
	}
	w := wire.toWhoIs()
	w.CapMap = nil // not destination-scoped through the CLI; never grant from it
	return w, nil
}

func (p *cli) Self(ctx context.Context) (Self, error) {
	out, err := p.run(ctx, "status", "--json")
	if err != nil {
		return Self{}, err
	}
	var wire statusWire
	if err := json.Unmarshal(out, &wire); err != nil {
		return Self{}, fmt.Errorf("decoding tailscale status: %w", err)
	}
	return wire.toSelf()
}

func (p *cli) SelfIPs(ctx context.Context) ([]string, error) {
	out, err := p.run(ctx, "ip")
	if err != nil {
		return nil, err
	}
	var ips []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" && net.ParseIP(line) != nil {
			ips = append(ips, line)
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("tailscale ip reported no addresses; is Tailscale up?")
	}
	return ips, nil
}

// PreferredIP picks an IPv4 tailnet address when present, else the first.
func PreferredIP(ips []string) (string, error) {
	var v6 string
	for _, raw := range ips {
		ip := net.ParseIP(raw)
		if ip == nil {
			continue
		}
		if ip.To4() != nil {
			return ip.String(), nil
		}
		if v6 == "" {
			v6 = ip.String()
		}
	}
	if v6 != "" {
		return v6, nil
	}
	return "", errors.New("no usable tailnet address")
}
