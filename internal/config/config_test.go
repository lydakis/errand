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
	if !cfg.ApplyOnSuccess {
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

	got, err := ResolveListen("tailnet:7443", socket)
	if err != nil {
		t.Fatal(err)
	}
	if got != "100.101.102.103:7443" {
		t.Fatalf("resolved tailnet listener = %q, want %q", got, "100.101.102.103:7443")
	}
}

func TestResolveListenLeavesExplicitAddressAlone(t *testing.T) {
	got, err := ResolveListen("127.0.0.1:7443", filepath.Join(t.TempDir(), "missing.sock"))
	if err != nil || got != "127.0.0.1:7443" {
		t.Fatalf("explicit listener = %q, %v", got, err)
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

	if got, err := ResolveListen("tailnet:7443", socket); err == nil {
		t.Fatalf("tailnet listener without an assigned Tailscale IP = %q, want an error", got)
	}
}
