package config

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/lydakis/errand/internal/tailnet"
)

func TestConfigDirFailsClosedWithoutAbsoluteUserRoot(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if got, err := dir(); err == nil {
		t.Fatalf("config dir without HOME = %q, want an error", got)
	}

	t.Setenv("XDG_CONFIG_HOME", filepath.Join("relative", "config"))
	if got, err := dir(); err == nil {
		t.Fatalf("config dir from relative XDG_CONFIG_HOME = %q, want an error", got)
	}
}

func TestLoadDaemonFailsClosedWithoutAbsoluteStateRoot(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	configPath := filepath.Join(t.TempDir(), "errandd.toml")
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDaemon(configPath); err == nil {
		t.Fatal("daemon config without state_dir or HOME succeeded, want an error")
	}

	t.Setenv("HOME", filepath.Join("relative", "home"))
	if got, err := LoadDaemon(configPath); err == nil {
		t.Fatalf("daemon config from relative HOME used state dir %q, want an error", got.StateDir)
	}
}

func TestLoadClientApplyOnSuccess(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configDir, err := dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("apply_on_success = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ApplyOnSuccess == nil || !*cfg.ApplyOnSuccess {
		t.Fatal("apply_on_success was not loaded")
	}
}

func TestLoadDaemonRejectsMissingExplicitConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.toml")
	if _, err := LoadDaemon(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadDaemon(%q) error = %v, want not-exist error", path, err)
	}
}

func TestLoadDaemonAllowsMissingDefaultConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	got, err := LoadDaemon("")
	if err != nil {
		t.Fatal(err)
	}
	if got.Listen != "tailnet:7443" || got.StateDir != filepath.Join(home, ".errand") {
		t.Fatalf("default daemon config = %+v", got)
	}
	if got.MaxJobs != 1 || got.MaxQueued != 8 {
		t.Fatalf("default concurrency = %d running, %d queued; want 1 and 8", got.MaxJobs, got.MaxQueued)
	}
}

func TestLoadDaemonPreservesExplicitZeroQueueCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "errandd.toml")
	config := "state_dir = \"/tmp/errand-test-state\"\nmax_queued = 0\n"
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxJobs != 1 || got.MaxQueued != 0 {
		t.Fatalf("explicit zero queue config = %d running, %d queued; want 1 and 0", got.MaxJobs, got.MaxQueued)
	}
}

func TestLoadDaemonRejectsNegativeQueueCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "errandd.toml")
	config := "state_dir = \"/tmp/errand-test-state\"\nmax_queued = -1\n"
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDaemon(path); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("negative max_queued error = %v", err)
	}
}

func TestLoadDaemonRejectsCacheTTLThatOverflowsDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "errandd.toml")
	config := "state_dir = \"/tmp/errand-test-state\"\n[cache]\nttl_hours = 3000000\n"
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDaemon(path); err == nil {
		t.Fatal("cache TTL exceeding time.Duration was accepted")
	}
}

func TestResolveListenUsesTailscaleLocalAPIAddress(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "errand-config-test-")
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
		if r.URL.Path != "/localapi/v0/status" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"BackendState": "Running",
			"TailscaleIPs": []string{"100.101.102.103", "fd7a:115c:a1e0::1"},
		})
	})}
	go server.Serve(listener)
	t.Cleanup(func() {
		server.Close()
		listener.Close()
	})

	got, err := ResolveListen("tailnet:7443", tailnet.NewLocalAPI(socket).SelfIPs)
	if err != nil {
		t.Fatal(err)
	}
	if got != "100.101.102.103:7443" {
		t.Fatalf("resolved tailnet listener = %q, want %q", got, "100.101.102.103:7443")
	}
}

func TestResolveListenRejectsLoopback(t *testing.T) {
	for _, address := range []string{"127.0.0.1:7443", "[::1]:7443", "localhost:7443"} {
		if got, err := ResolveListen(address, nil); err == nil {
			t.Fatalf("loopback listener %q resolved to %q, want an error", address, got)
		}
	}
}

func TestResolveListenLeavesExplicitRemoteAddressAlone(t *testing.T) {
	const address = "100.101.102.103:7443"
	got, err := ResolveListen(address, nil)
	if err != nil || got != address {
		t.Fatalf("explicit listener = %q, %v", got, err)
	}
}

func TestResolveListenCanDisableTCP(t *testing.T) {
	got, err := ResolveListen("none", nil)
	if err != nil || got != "" {
		t.Fatalf("disabled TCP listener = %q, %v", got, err)
	}
}

func TestResolveListenFailsClosedWithoutAssignedTailscaleIP(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "errand-config-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "tailscaled.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"BackendState": "NeedsLogin",
			"TailscaleIPs": []string{},
		})
	})}
	go server.Serve(listener)
	t.Cleanup(func() {
		server.Close()
		listener.Close()
	})

	if got, err := ResolveListen("tailnet:7443", tailnet.NewLocalAPI(socket).SelfIPs); err == nil {
		t.Fatalf("tailnet listener without an assigned Tailscale IP = %q, want an error", got)
	}
}

func TestSSHPeersResolveToSSHURLsWithoutLeakingTheCommand(t *testing.T) {
	c := Client{DefaultPeer: "mini", Peers: map[string]Peer{
		"mini":    {SSH: "george@mini", RemoteCommand: "/home/george/.local/bin/errand", RemoteSocket: "/srv/errand/runner.sock"},
		"both":    {SSH: "mini", URL: "http://mini:7443"},
		"weird":   {SSH: "mini/../x"},
		"secret":  {SSH: "george:password@mini"},
		"socket":  {SSH: "mini", RemoteSocket: "relative.sock"},
		"command": {SSH: "mini", RemoteCommand: "~/bin/errand"},
	}}
	u, err := c.PeerURL("")
	if err != nil || u != "ssh://george@mini" {
		t.Fatalf("ssh peer url = %q, %v", u, err)
	}
	if got := c.SSHRemoteCommand(""); got != "/home/george/.local/bin/errand" {
		t.Fatalf("remote command = %q", got)
	}
	if got := c.SSHRemoteSocket(""); got != "/srv/errand/runner.sock" {
		t.Fatalf("remote socket = %q", got)
	}
	if _, err := c.PeerURL("both"); err == nil {
		t.Fatal("a peer with both url and ssh must be rejected")
	}
	if _, err := c.PeerURL("weird"); err == nil {
		t.Fatal("ssh host with path characters must be rejected")
	}
	if _, err := c.PeerURL("secret"); err == nil {
		t.Fatal("ssh target with password syntax must be rejected")
	}
	if _, err := c.PeerURL("socket"); err == nil {
		t.Fatal("a relative remote socket must be rejected")
	}
	if _, err := c.PeerURL("command"); err == nil {
		t.Fatal("a non-absolute remote command must be rejected")
	}
}

func TestAddPeerRejectsDefinitionsThatCannotBeResolved(t *testing.T) {
	for name, peer := range map[string]Peer{
		"bad-url":     {URL: "ftp://runner"},
		"url-command": {URL: "http://runner:7443", RemoteCommand: "/bin/errand"},
		"bad-ssh":     {SSH: "runner with spaces"},
		"bad-command": {SSH: "runner", RemoteCommand: "bin/errand"},
		"bad-socket":  {SSH: "runner", RemoteSocket: "run/errand.sock"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if _, err := AddPeer(path, name, peer, false); err == nil {
				t.Fatal("AddPeer accepted an unusable peer")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("AddPeer wrote an unusable peer")
			}
		})
	}
}

func TestAddPeerPreservesExistingConfigWhenSettingFirstDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "# keep this comment\napply_on_success = true\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	madeDefault, err := AddPeer(path, "cabal", Peer{URL: "http://cabal:7443"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !madeDefault {
		t.Fatal("first peer was not made default")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{`default_peer = "cabal"`, "# keep this comment", "apply_on_success = true", "[peers.cabal]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing %q:\n%s", want, text)
		}
	}
	var cfg Client
	if _, err := toml.Decode(text, &cfg); err != nil {
		t.Fatalf("written config is invalid: %v\n%s", err, text)
	}
}

func TestAddPeerExtendsInlinePeerConfigWithoutCorruptingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := `default_peer = "existing"
peers = { existing = { url = "http://existing:7443" } }
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AddPeer(path, "cabal", Peer{URL: "http://cabal:7443"}, false); err != nil {
		t.Fatal(err)
	}
	var cfg Client
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		t.Fatalf("AddPeer wrote invalid TOML: %v", err)
	}
	if cfg.DefaultPeer != "existing" || cfg.Peers["existing"].URL != "http://existing:7443" || cfg.Peers["cabal"].URL != "http://cabal:7443" {
		t.Fatalf("config after AddPeer = %+v", cfg)
	}
}
