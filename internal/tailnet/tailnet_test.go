package tailnet

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSupportsDestinationScopedWhoIs(t *testing.T) {
	for version, want := range map[string]bool{
		"1.100.0": true, "v1.102.3": true, "2.0.0": true,
		"1.99.9": false, "1.9": false, "": false, "garbage": false,
	} {
		if got := SupportsDestinationScopedWhoIs(version); got != want {
			t.Errorf("SupportsDestinationScopedWhoIs(%q) = %v, want %v", version, got, want)
		}
	}
}

func fakeLocalAPI(t *testing.T, version string, whois map[string]any) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "errand-tailnet-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "tailscaled.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if version != "" {
			w.Header().Set("Tailscale-Version", version)
		}
		switch r.URL.Path {
		case "/localapi/v0/whois":
			if r.URL.Query().Get("dst_ip") == "" {
				http.Error(w, "missing dst_ip", http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(whois)
		case "/localapi/v0/status":
			json.NewEncoder(w).Encode(map[string]any{
				"BackendState": "Running", "TailscaleIPs": []string{"fd7a::1", "100.64.0.9"},
			})
		default:
			http.NotFound(w, r)
		}
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close(); listener.Close() })
	return socket
}

var fakeWhois = map[string]any{
	"Node":        map[string]any{"Name": "cabal.example.ts.net.", "StableID": "nABC"},
	"UserProfile": map[string]any{"ID": 42, "LoginName": "george@example.com"},
	"CapMap":      map[string]any{"example.com/cap/errand": []map[string]any{{"actions": []string{"submit"}}}},
}

func TestLocalAPIWhoIsIsDestinationScopedAndVersionGated(t *testing.T) {
	p := NewLocalAPI(fakeLocalAPI(t, "1.102.3", fakeWhois))
	w, err := p.WhoIs(context.Background(), "100.64.0.1:1234", "100.64.0.9")
	if err != nil {
		t.Fatal(err)
	}
	if w.LoginName != "george@example.com" || w.UserID != 42 || w.NodeStableID != "nABC" || w.CapMap == nil {
		t.Fatalf("whois = %+v", w)
	}
	if ip, err := PreferredIP(mustIPs(t, p)); err != nil || ip != "100.64.0.9" {
		t.Fatalf("preferred ip = %q, %v (want IPv4 first)", ip, err)
	}

	old := NewLocalAPI(fakeLocalAPI(t, "1.98.0", fakeWhois))
	if _, err := old.WhoIs(context.Background(), "100.64.0.1:1234", "100.64.0.9"); err == nil ||
		!strings.Contains(err.Error(), "1.100") {
		t.Fatalf("old tailscaled must fail closed, got %v", err)
	}
}

func mustIPs(t *testing.T, p Provider) []string {
	t.Helper()
	ips, err := p.SelfIPs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ips
}

func fakeCLI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tailscale")
	script := `#!/bin/sh
case "$1" in
  whois) printf '%s' '{"Node":{"Name":"cabal.example.ts.net.","StableID":"nABC"},"UserProfile":{"ID":42,"LoginName":"george@example.com"},"CapMap":{"example.com/cap/errand":[{"actions":["submit"]}]}}' ;;
  ip) printf '100.64.0.9\nfd7a::1\n' ;;
  *) echo "unknown" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCLIProviderParsesOutputAndDropsUnscopedCapabilities(t *testing.T) {
	p := NewCLI(fakeCLI(t))
	w, err := p.WhoIs(context.Background(), "100.64.0.1:1234", "100.64.0.9")
	if err != nil {
		t.Fatal(err)
	}
	if w.LoginName != "george@example.com" || w.UserID != 42 {
		t.Fatalf("cli whois = %+v", w)
	}
	if w.CapMap != nil {
		t.Fatal("cli whois is not destination-scoped; CapMap must be dropped so nothing is granted from it")
	}
	if ip, err := PreferredIP(mustIPs(t, p)); err != nil || ip != "100.64.0.9" {
		t.Fatalf("cli preferred ip = %q, %v", ip, err)
	}
}

func TestDiscoverPrefersExplicitConfigAndNamesWhatItTried(t *testing.T) {
	socket := fakeLocalAPI(t, "1.102.3", fakeWhois)
	if p, err := Discover(socket, ""); err != nil || !strings.HasPrefix(p.Name(), "localapi:") {
		t.Fatalf("explicit socket discovery = %v, %v", p, err)
	}
	if p, err := Discover("", fakeCLI(t)); err != nil || !strings.HasPrefix(p.Name(), "cli:") {
		t.Fatalf("explicit cli discovery = %v, %v", p, err)
	}
	if _, err := Discover(filepath.Join(t.TempDir(), "missing.sock"), ""); err == nil {
		t.Fatal("a missing explicit socket must be an error, not a silent fallback")
	}
	t.Setenv("PATH", t.TempDir()) // no tailscale binary reachable
	if _, err := Discover("", ""); err == nil || !strings.Contains(err.Error(), "tried") {
		// default sockets may exist on a developer machine; only assert the message shape on failure
		if err != nil {
			t.Fatalf("discovery failure must name what it tried: %v", err)
		}
	}
}

func TestDefaultDiscoveryFallsBackFromStaleSocketToCLI(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "errand-tailnet-stale-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "tailscaled.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	cli := fakeCLI(t)
	t.Setenv("PATH", filepath.Dir(cli))

	provider, err := discoverDefault([]string{socket})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(provider.Name(), "cli:") {
		t.Fatalf("provider = %s, want CLI fallback", provider.Name())
	}
}
