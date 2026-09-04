package daemon

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func unixDaemon(t *testing.T, cfg Config) (*Daemon, *http.Client) {
	t.Helper()
	cfg.StateDir = t.TempDir()
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	dir, err := os.MkdirTemp("/tmp", "errand-unix-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "errand.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: d.Handler(), ConnContext: ConnContext}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close(); listener.Close() })
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socket)
		},
	}}
	return d, client
}

func TestUnixSocketCallerIsIdentifiedByKernelCredentials(t *testing.T) {
	d, client := unixDaemon(t, Config{})
	resp, err := client.Get("http://errand/v0/info")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("own user over the socket = %s, want 200 (default allow is the daemon's uid)", resp.Status)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://errand/v0/jobs", nil)
	listResp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("owned listing over the socket = %s", listResp.Status)
	}
	id, err := d.identifyLocal(LocalPeer{UID: d.selfUID, User: "me"})
	if err != nil {
		t.Fatal(err)
	}
	if !id.Local || id.Method != "local" || !strings.HasPrefix(id.Owner(), "local-user:") {
		t.Fatalf("local identity = %+v owner %q", id, id.Owner())
	}
	if got := admissionOwner(proto.Admission{Method: "local", LocalUID: int64(d.selfUID), LocalUser: "me"}); got != id.Owner() {
		t.Fatalf("admission owner %q != identity owner %q", got, id.Owner())
	}
}

func TestUnixSocketRefusesAnotherLocalUser(t *testing.T) {
	d, _ := unixDaemon(t, Config{})
	if _, err := d.identifyLocal(LocalPeer{UID: d.selfUID + 1, User: "someone-else"}); err == nil {
		t.Fatal("identifyLocal accepted a uid other than the daemon owner")
	}
}

func TestTailnetPathStillRequiresAProvider(t *testing.T) {
	d, err := New(Config{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.identify("100.64.0.1:1234", testDestination()); err == nil ||
		!strings.Contains(err.Error(), "no tailnet identity provider") {
		t.Fatalf("tcp caller without a provider = %v, want fail-closed refusal", err)
	}
}
