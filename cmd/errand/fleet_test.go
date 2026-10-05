package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/daemon"
)

func TestFleetReadReportsUnreachablePeersAfterResults(t *testing.T) {
	d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	server := httptest.NewServer(d.Handler())
	defer server.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gone := "http://" + ln.Addr().String()
	ln.Close()
	writeClientConfig(t, fmt.Sprintf("default_peer = 'cabal'\n[peers.cabal]\nurl = %q\n[peers.gone]\nurl = %q\n", server.URL, gone))
	t.Chdir(t.TempDir())
	var out bytes.Buffer
	if code := cmdWorkspacesTo([]string{"create", "--on", "cabal", "--no-snapshot", "scratch"}, &out, &out); code != 0 {
		t.Fatalf("create: %d %s", code, &out)
	}

	out.Reset()
	if code := cmdWorkspacesTo(nil, &out, &out); code != 1 {
		t.Fatalf("list: %d %s", code, &out)
	}
	text := out.String()
	row := strings.Index(text, "scratch")
	failure := strings.Index(text, "errand: peer gone: unreachable: connection refused")
	if row < 0 || failure < row {
		t.Fatalf("want the reachable peer's rows, then the unreachable peer:\n%s", text)
	}
}

func TestDescribePeerErrorSeparatesUnreachableFromSlow(t *testing.T) {
	unreachable := &url.Error{Op: "Get", URL: "http://100.64.0.3:7443/v0/jobs", Err: &client.UnreachableError{Reason: "Tailscale reports cabal offline, last seen 3h ago"}}
	if got := describePeerError(unreachable); got != "unreachable: Tailscale reports cabal offline, last seen 3h ago" {
		t.Fatalf("unreachable: %q", got)
	}
	slow := &url.Error{Op: "Get", URL: "http://mini:7443/v0/jobs", Err: context.DeadlineExceeded}
	if got := describePeerError(slow); !strings.HasPrefix(got, "timed out: connected") {
		t.Fatalf("slow: %q", got)
	}
}
