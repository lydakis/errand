package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalPeerUsesSavedSocketAndExplicitSelection(t *testing.T) {
	root := runFixture(t, "default_peer = 'remote'\n[peers.remote]\nurl = 'http://remote:7443'\n", "[run]\npeer = 'local'\n[profiles.experiment.run]\npeer = 'local'\n")
	path, _ := DaemonPath()
	if err := os.WriteFile(path, []byte("transport = 'local'\nsocket = '/tmp/custom local.sock'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveRun(root, RunOverrides{}); err == nil {
		t.Fatal("workspace selected unconfigured local authority")
	}
	want, _ := LocalURL("/tmp/custom local.sock")
	for _, opts := range []RunOverrides{{Peer: "local"}, {Profile: "experiment"}} {
		got, err := ResolveRun(root, opts)
		if err != nil || got.URL != want || got.Peer != "local" {
			t.Fatalf("local resolution: %+v %v", got, err)
		}
	}
	c, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.WithLocalPeer().Peers["local"]; !ok {
		t.Fatal("installed local runner omitted from inventory")
	}
	if _, ok := c.Peers["local"]; ok {
		t.Fatal("inventory mutated personal configuration")
	}
	if c.DefaultPeer != "remote" {
		t.Fatal("local runner changed default")
	}
	if err := os.WriteFile(path, []byte("transport = 'tailscale'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.WithLocalPeer().Peers["local"]; ok {
		t.Fatal("inventory added a local target that refuses job APIs")
	}
}

func TestLocalPeerConfiguration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := (Client{}).PeerURL(""); err == nil {
		t.Fatal("local became implicit fallback")
	}
	if _, err := (Client{}).PeerURL("local"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []Peer{{URL: "http://remote:7443"}, {SSH: "remote"}, {RemoteCommand: "/bin/errand"}, {RemoteSocket: "/tmp/socket"}, {Socket: "relative"}} {
		if _, err := (Client{Peers: map[string]Peer{"local": p}}).PeerURL("local"); err == nil {
			t.Fatalf("accepted ambiguous local peer %+v", p)
		}
	}
	c := Client{DefaultPeer: "sandbox", Peers: map[string]Peer{"sandbox": {Socket: filepath.Join(t.TempDir(), "runner.sock")}}}
	if u, err := c.PeerURL(""); err != nil || !strings.HasPrefix(u, "unix://") {
		t.Fatalf("socket alias: %s %v", u, err)
	}
	if err := ValidatePeer("sandbox", Peer{Socket: "/tmp/a", URL: "http://remote:7443"}); err == nil || strings.Contains(err.Error(), "rename") {
		t.Fatalf("wrong mixed-transport diagnostic: %v", err)
	}
}

func TestLocalPeerSurvivesPersonalConfigurationEdits(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, _ := ClientPath()
	if _, err := AddPeer(path, "remote", Peer{URL: "http://remote:7443"}, false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sandbox", "local"} {
		peer := Peer{}
		if name == "sandbox" {
			peer.Socket = "/tmp/errand.sock"
		}
		if _, err := AddPeer(path, name, peer, false); err != nil {
			t.Fatal(err)
		}
	}
	// Removal re-encodes the document and must preserve both local forms.
	if _, err := RemovePeer(path, "remote"); err != nil {
		t.Fatal(err)
	}
	c, err := LoadClient()
	if err != nil || c.Peers["sandbox"].Socket != "/tmp/errand.sock" {
		t.Fatalf("lost socket: %+v %v", c, err)
	}
	if _, ok := c.Peers["local"]; !ok {
		t.Fatal("lost explicit local preference")
	}
}

func TestWorkspaceLocalSelectionRequiresPersonalOrSelectedProfileConsent(t *testing.T) {
	for _, tc := range []struct {
		name, personal, profile string
		allowed                 bool
	}{
		{name: "personal default", personal: "default_peer = 'local'", allowed: true},
		{name: "personal alias", personal: "[peers.local]", allowed: true},
		{name: "unrelated profile", profile: "unrelated", allowed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := runFixture(t, tc.personal, "[run]\npeer = 'local'\n[profiles.unrelated.run]\nworkdir = '.'\n")
			_, err := ResolveRun(root, RunOverrides{Profile: tc.profile})
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v, err=%v", tc.allowed, err)
			}
		})
	}
}
