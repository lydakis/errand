package client

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/tailnet"
)

func TestSSHConnectFailureIsUnreachable(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho 'ssh: connect to host gone port 22: Operation timed out' >&2\nexit 255\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	_, err := ProbeInfo(context.Background(), ConfigureSSHPeer("ssh://gone", "gone", "", ""), 4*time.Second)
	if kind, _ := ProbeKindOf(err); kind != ProbeUnreachable || err.Error() != "unreachable: ssh could not connect to gone" {
		t.Fatalf("err = %v", err)
	}
}

func TestSSHToTailscaleOfflineNodeFailsFast(t *testing.T) {
	fakeReach(t, 5*time.Second, []tailnet.Peer{{HostName: "cabal", LastSeen: time.Now().Add(-2 * time.Hour), IPs: []string{"100.64.0.5"}}})
	bin := t.TempDir()
	// ssh -G resolves the alias to a tailnet address; a real connect hangs
	// the way a SYN to a vanished host does.
	script := "#!/bin/sh\nif [ \"$1\" = -G ]; then printf 'user me\\nhostname 100.64.0.5\\nport 22\\n'; exit 0; fi\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	start := time.Now()
	_, err := ProbeInfo(context.Background(), ConfigureSSHPeer("ssh://cabal", "cabal", "", ""), 4*time.Second)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("offline SSH peer took %s", elapsed)
	}
	if kind, _ := ProbeKindOf(err); kind != ProbeUnreachable || err.Error() != "unreachable: Tailscale reports cabal offline, last seen 2h ago" {
		t.Fatalf("err = %v", err)
	}
}

func TestSSHHostAddrFollowsSSHConfig(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf 'hostname box.example.ts.net\\nport 2222\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if addr, err := sshHostAddr(context.Background(), "alias"); err != nil || addr != "box.example.ts.net:2222" {
		t.Fatalf("addr = %q, err = %v", addr, err)
	}
}

func TestSSHKeepsSlowConnectionToNodeWithLiveDataPath(t *testing.T) {
	// The connection is up (fresh handshake) but the runner is slow to
	// answer, and the control plane has lost the node: nothing is cut.
	fakeReach(t, 5*time.Second, []tailnet.Peer{{HostName: "cabal", LastHandshake: time.Now(), IPs: []string{"100.64.0.5"}}})
	bin := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = -G ]; then printf 'hostname 100.64.0.5\\nport 22\\n'; exit 0; fi\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	_, err := ProbeInfo(context.Background(), ConfigureSSHPeer("ssh://cabal", "cabal", "", ""), time.Second)
	if kind, _ := ProbeKindOf(err); kind != ProbeUnreachable || err.Error() != "unreachable: timed out" {
		t.Fatalf("err = %v", err)
	}
}
