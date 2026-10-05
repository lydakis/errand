package client

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/tailnet"
)

// fakeReach replaces the network and tailscaled for one test: every dial
// hangs until its context ends, as a SYN to a vanished host does.
func fakeReach(t *testing.T, budget time.Duration, peers []tailnet.Peer) *atomic.Int32 {
	t.Helper()
	oldBudget, oldAfter, oldDial, oldPeers := peerConnectTimeout, tailnetCheckAfter, dialTCP, tailnetPeers
	t.Cleanup(func() {
		peerConnectTimeout, tailnetCheckAfter, dialTCP, tailnetPeers = oldBudget, oldAfter, oldDial, oldPeers
	})
	peerConnectTimeout, tailnetCheckAfter = budget, 10*time.Millisecond
	dialTCP = func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: ctx.Err()}
	}
	var lookups atomic.Int32
	tailnetPeers = func(context.Context) ([]tailnet.Peer, error) {
		lookups.Add(1)
		return peers, nil
	}
	return &lookups
}

func TestDialPeerFailsFastForTailscaleOfflineNode(t *testing.T) {
	fakeReach(t, 5*time.Second, []tailnet.Peer{
		{HostName: "mini", Online: true, IPs: []string{"100.64.0.2"}},
		{HostName: "cabal", Online: false, LastSeen: time.Now().Add(-3 * time.Hour), IPs: []string{"100.64.0.3", "fd7a:115c:a1e0::3"}},
	})
	for _, addr := range []string{"100.64.0.3:7443", "[fd7a:115c:a1e0::3]:7443"} {
		start := time.Now()
		_, err := dialPeer(context.Background(), "tcp", addr)
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("%s: offline node took %s", addr, elapsed)
		}
		if !IsUnreachable(err) || err.Error() != "unreachable: Tailscale reports cabal offline, last seen 3h ago" {
			t.Fatalf("%s: %v", addr, err)
		}
	}
}

func TestDialPeerGivesOnlineNodeItsConnectBudget(t *testing.T) {
	lookups := fakeReach(t, 100*time.Millisecond, []tailnet.Peer{{HostName: "mini", Online: true, IPs: []string{"100.64.0.2"}}})
	start := time.Now()
	_, err := dialPeer(context.Background(), "tcp", "100.64.0.2:7443")
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("connect budget not applied: %s", elapsed)
	}
	if !IsUnreachable(err) || !strings.Contains(err.Error(), "no answer within 100ms") {
		t.Fatalf("err = %v", err)
	}
	if lookups.Load() != 1 {
		t.Fatalf("tailscaled lookups = %d", lookups.Load())
	}
}

func TestDialPeerAsksTailscaleOnlyAboutSlowTailnetConnections(t *testing.T) {
	lookups := fakeReach(t, 50*time.Millisecond, nil)
	if _, err := dialPeer(context.Background(), "tcp", "192.0.2.1:7443"); !IsUnreachable(err) {
		t.Fatalf("err = %v", err)
	}
	server, client := net.Pipe()
	defer server.Close()
	dialTCP = func(context.Context, string, string) (net.Conn, error) { return client, nil }
	conn, err := dialPeer(context.Background(), "tcp", "100.64.0.2:7443")
	if err != nil || conn != client {
		t.Fatalf("conn = %v, err = %v", conn, err)
	}
	time.Sleep(30 * time.Millisecond) // past tailnetCheckAfter
	if lookups.Load() != 0 {
		t.Fatalf("tailscaled lookups = %d", lookups.Load())
	}
}

func TestDialPeerKeepsCallerDeadline(t *testing.T) {
	fakeReach(t, 5*time.Second, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := dialPeer(ctx, "tcp", "192.0.2.1:7443")
	if IsUnreachable(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestDialPeerNamesRefusedConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, err = dialPeer(context.Background(), "tcp", addr)
	if !IsUnreachable(err) || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v", err)
	}
}

func TestProbeReportsTailscaleOfflineNode(t *testing.T) {
	fakeReach(t, 5*time.Second, []tailnet.Peer{{DNSName: "box.example.ts.net", IPs: []string{"100.64.0.9"}}})
	_, err := ProbeInfo(context.Background(), "http://100.64.0.9:7443", 4*time.Second)
	if kind, _ := ProbeKindOf(err); kind != ProbeUnreachable || err.Error() != "unreachable: Tailscale reports box.example.ts.net offline" {
		t.Fatalf("err = %v", err)
	}
}
