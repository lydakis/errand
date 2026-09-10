//go:build darwin || linux

package setup

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/tailnet"
)

// Network coverage is explicitly selected: this submits disposable jobs through
// an existing trusted peer and reaches the isolated service over real Tailscale.
// It never disables authentication or alters firewall settings.
func liveNetworkConfig(t *testing.T, ctx context.Context, home string) (string, string) {
	t.Helper()
	provider, err := tailnet.Discover("", "")
	if err != nil {
		t.Fatal(err)
	}
	self, err := provider.Self(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ip := ""
	for _, candidate := range self.IPs {
		if net.ParseIP(candidate).To4() != nil {
			ip = candidate
			break
		}
	}
	if ip == "" || self.Login == "" {
		t.Fatal("network fixture requires an authenticated IPv4 tailnet node")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	initial := fmt.Sprintf("transport = 'both'\nlisten = %q\nallow_users = [%q]\nstate_dir = %q\nmax_jobs = 2\nmax_queued = 3\n", address, self.Login, filepath.Join(home, "state"))
	return initial, "http://" + address
}

func liveRemoteCommand(ctx context.Context, peer string, argv ...string) *exec.Cmd {
	args := append([]string{"--on", peer, "--no-snapshot", "--no-apply", "--no-forward", "--no-artifacts", "--no-caches", "--"}, argv...)
	return exec.CommandContext(ctx, "errand", args...)
}

func liveRemoteProbe(t *testing.T, ctx context.Context, peer, target, version string) {
	t.Helper()
	out, err := liveRemoteCommand(ctx, peer, "curl", "--fail", "--max-time", "10", "--silent", "--show-error", target+"/v0/info").CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte(`"version":"`+version+`"`)) {
		t.Fatalf("cross-machine info probe failed: %v / %s", err, out)
	}
}

func liveRemoteBusyJob(ctx context.Context, peer, target, release, home string) *exec.Cmd {
	script := `set -eu
handle=$(errand --url "$1" --no-snapshot --no-apply --no-caches --artifact proof.txt -d -- /bin/sh -c 'while [ -d "$2" ] && [ ! -e "$1" ]; do sleep 0.05; done; printf retained > proof.txt' wait "$2" "$3")
errand attach "$handle"
errand fetch --output "$PWD/proof" "$handle"
test "$(cat "$PWD/proof/proof.txt")" = retained
printf remote-result-ok
`
	return liveRemoteCommand(ctx, peer, "/bin/sh", "-c", script, "upgrade-proof", target, release, home)
}

func requireLiveFirewall(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := os.Stat("/usr/libexec/ApplicationFirewall/socketfilterfw"); err != nil {
		return
	}
	out, err := exec.CommandContext(ctx, "/usr/libexec/ApplicationFirewall/socketfilterfw", "--getglobalstate").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "enabled") {
		t.Fatalf("network upgrade acceptance requires the existing macOS firewall enabled; test does not change it: %s / %v", out, err)
	}
	t.Log("macOS Application Firewall is enabled; settings unchanged")
}
