package tailnet

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

const statusDoc = `{"Version":"1.102.3-t9329c3677","BackendState":"Running",
"Self":{"DNSName":"cabal.example.ts.net.","HostName":"cabal","UserID":42,"TailscaleIPs":["100.64.0.9","fd7a::1"],"OS":"linux"},
"User":{"42":{"LoginName":"george@example.com"}}}`

func TestSelfFromLocalAPI(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "errand-self-")
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
		w.Header().Set("Tailscale-Version", "1.102.3")
		w.Write([]byte(statusDoc))
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close(); listener.Close() })

	self, err := NewLocalAPI(socket).Self(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if self.Login != "george@example.com" || self.DNSName != "cabal.example.ts.net" ||
		self.HostName != "cabal" || self.UserID != 42 || self.OS != "linux" || self.Version == "" {
		t.Fatalf("self = %+v", self)
	}
	if !SupportsDestinationScopedWhoIs(self.Version) {
		t.Fatalf("version %q should satisfy the gate", self.Version)
	}
}

func TestSelfFromCLI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tailscale")
	script := "#!/bin/sh\ncase \"$1\" in status) printf '%s' '" + statusDoc + "' ;; *) exit 2 ;; esac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := NewCLI(path).Self(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if self.Login != "george@example.com" || self.DNSName != "cabal.example.ts.net" {
		t.Fatalf("cli self = %+v", self)
	}
}

func TestSelfRefusesLoggedOutNode(t *testing.T) {
	var wire statusWire
	if err := json.Unmarshal([]byte(`{"BackendState":"NeedsLogin","Self":{"UserID":0},"User":{}}`), &wire); err != nil {
		t.Fatal(err)
	}
	if _, err := wire.toSelf(); err == nil {
		t.Fatal("a node with no login must not produce a Self")
	}
}
