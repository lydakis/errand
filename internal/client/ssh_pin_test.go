package client

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

const testHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOFmZUN5Kd0kLHRZhkmAlu0HQK9h5BFXKCzIdeUCC5n3 errand-lease"

func pinArgs(t *testing.T, target string) []string {
	t.Helper()
	args, err := sshPinArgs(target)
	if err != nil {
		t.Fatal(err)
	}
	return args
}

// freshSSHTrust has a test start from a process that trusts nothing yet.
func freshSSHTrust(t *testing.T) {
	sshTrustMu.Lock()
	saved := sshTrusted
	sshTrusted = map[string]sshTrust{}
	sshTrustMu.Unlock()
	t.Cleanup(func() {
		sshTrustMu.Lock()
		sshTrusted = saved
		sshTrustMu.Unlock()
	})
}

func TestSSHPinArgs(t *testing.T) {
	freshSSHTrust(t)
	cache := filepath.Join(t.TempDir(), "cache dir") // ssh splits unquoted paths at spaces
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	target := "ubuntu@203.0.113.7"
	if args := pinArgs(t, target); args != nil {
		t.Fatalf("untrusted target got %q", args)
	}
	for _, bad := range []string{"ssh-ed25519", "ssh-ed25519 not*base64", "ssh-ed25519 AAAA\n@cert-authority * ssh-ed25519 AAAA", "SSH ED AAAA"} {
		if err := TrustSSHHost(target, bad, ""); err == nil {
			t.Fatalf("trusted %q", bad)
		}
	}
	if err := TrustSSHHost(target, testHostKey, "relative/key"); err == nil {
		t.Fatal("trusted a relative identity file")
	}
	if err := TrustSSHHost(target, testHostKey, ""); err != nil {
		t.Fatal(err)
	}
	if args := pinArgs(t, target); !slices.Contains(args, "StrictHostKeyChecking=yes") || slices.Contains(args, "-i") || slices.Contains(args, "IdentitiesOnly=yes") {
		t.Fatalf("args %q", args)
	}
	// A cloud peer and a client in one process each offer their own key.
	for _, identity := range []string{"/keys/lambda", "/keys/errand", "/keys/lambda"} {
		if err := TrustSSHHost(target, testHostKey, identity); err != nil {
			t.Fatal(err)
		}
	}
	args := pinArgs(t, target)
	// Only those keys: others from an agent could use up the server's
	// authentication attempts first.
	if want := []string{"-o", "IdentitiesOnly=yes", "-i", "/keys/lambda", "-i", "/keys/errand"}; !slices.Equal(args[len(args)-6:], want) {
		t.Fatalf("args %q", args)
	}
	if pinArgs(t, "ubuntu@203.0.113.8") != nil {
		t.Fatal("trust leaked to another target")
	}
	if runtime.GOOS == "windows" {
		return
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh")
	}
	// ssh -G prints the options it would use, quoting and all.
	out, err := exec.Command("ssh", append(args, "-G", target)...).Output()
	if err != nil {
		t.Fatal(err)
	}
	config := string(out)
	for _, want := range []string{"hostkeyalias " + sshPinAlias(target), "stricthostkeychecking true", "globalknownhostsfile /dev/null"} {
		if !strings.Contains(config, want+"\n") {
			t.Errorf("ssh -G lacks %q", want)
		}
	}
	controlDir, _ := sshControlDirPath()
	known := regexp.MustCompile(`(?m)^userknownhostsfile (.*)$`).FindStringSubmatch(config)
	if known == nil || !strings.HasPrefix(known[1], filepath.Join(controlDir, "known-")) {
		t.Fatalf("ssh -G known hosts: %q", known)
	}
	// A control master opened without the key, or with another one, is
	// never shared with a pinned connection.
	pinned := regexp.MustCompile(`(?m)^controlpath (.*)$`).FindStringSubmatch(config)
	if pinned == nil || !strings.HasPrefix(pinned[1], filepath.Join(controlDir, "pin-")) {
		t.Fatalf("ssh -G control path: %q", pinned)
	}
	if err := TrustSSHHost(target, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOFmZUN5Kd0kLHRZhkmAlu0HQK9h5BFXKCzIdeUCC5n4 other", ""); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(pinArgs(t, target), `ControlPath="`+pinned[1]+`"`) {
		t.Fatal("a new host key reuses the old control path")
	}
}

// A later process applying a job's changes reaches a leased machine with the
// host key and identities the job was started with.
func TestRestoredSSHPeerKeepsHostKey(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	target := "ubuntu@203.0.113.9"
	restoreSSHPeer("ssh://peer-restored.errand", target, "", "", testHostKey, []string{"/keys/errand"})
	args := pinArgs(t, target)
	if !slices.Contains(args, "StrictHostKeyChecking=yes") || !slices.Contains(args, "/keys/errand") {
		t.Fatalf("args %q", args)
	}
}

// A target whose host key is not pinned still offers the lease's key, next
// to the user's own, with the user's known_hosts.
func TestSSHIdentityWithoutHostKey(t *testing.T) {
	freshSSHTrust(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	target := "ubuntu@203.0.113.21"
	if err := TrustSSHHost(target, "", "/keys/errand"); err != nil {
		t.Fatal(err)
	}
	if args := pinArgs(t, target); !slices.Equal(args, []string{"-i", "/keys/errand"}) {
		t.Fatalf("args %q", args)
	}
	// A host key learned later pins it, and keeps the identity.
	if err := TrustSSHHost(target, testHostKey, ""); err != nil {
		t.Fatal(err)
	}
	if args := pinArgs(t, target); !slices.Contains(args, "StrictHostKeyChecking=yes") || !slices.Contains(args, "/keys/errand") {
		t.Fatalf("args %q", args)
	}
	restoreSSHPeer("ssh://peer-unpinned.errand", "ubuntu@203.0.113.22", "", "", "", []string{"/keys/errand"})
	if args := pinArgs(t, "ubuntu@203.0.113.22"); !slices.Equal(args, []string{"-i", "/keys/errand"}) {
		t.Fatalf("restored args %q", args)
	}
}

// Connections to one target are pooled by what this process trusts about
// it: one opened before a lease's host key was pinned is not reused for the
// lease, while requests that trust the same keep sharing one connection.
func TestSSHPoolSeparatesTrust(t *testing.T) {
	freshSSHTrust(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	var mu sync.Mutex
	var dials []sshTrust
	oldDial := dialSSHConnection
	dialSSHConnection = func(ctx context.Context, _, _ string, trust sshTrust) (net.Conn, error) {
		mu.Lock()
		dials = append(dials, trust)
		mu.Unlock()
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	t.Cleanup(func() { dialSSHConnection = oldDial })
	target := "ubuntu@203.0.113.23"
	peer := ConfigureSSHPeer("ssh://"+target, "pool-test", "", "")
	rt := &sshRoundTripper{}
	get := func() {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, peer+"/v0/info", nil)
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	get()
	get()
	if err := TrustSSHHost(target, testHostKey, "/keys/errand"); err != nil {
		t.Fatal(err)
	}
	get()
	get()
	mu.Lock()
	defer mu.Unlock()
	if len(dials) != 2 || dials[0].hostKey != "" || dials[1].hostKey == "" || !slices.Equal(dials[1].identities, []string{"/keys/errand"}) {
		t.Fatalf("dials %+v", dials)
	}
}
