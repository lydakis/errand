package client

import (
	"context"
	"errors"
	"net"
	"net/netip"
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
	oldBudget, oldAfter, oldDial, oldPeers, oldResolve := peerConnectTimeout, tailnetCheckAfter, dialTCP, tailnetPeers, resolveHost
	t.Cleanup(func() {
		peerConnectTimeout, tailnetCheckAfter, dialTCP, tailnetPeers, resolveHost = oldBudget, oldAfter, oldDial, oldPeers, oldResolve
	})
	peerConnectTimeout, tailnetCheckAfter = budget, 10*time.Millisecond
	dialTCP = func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: ctx.Err()}
	}
	var lookups atomic.Int32
	started := time.Now()
	tailnetPeers = func(_ context.Context, notBefore time.Time) ([]tailnet.Peer, error) {
		// Each attempt must ask for a view from after it began.
		if notBefore.Before(started) {
			t.Errorf("lookup accepts a view from before the attempt (%s)", started.Sub(notBefore))
		}
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

func TestDialPeerKeepsTryingNodeWithLiveDataPath(t *testing.T) {
	// Offline to the coordination server, but this machine handshook with it
	// a minute ago: traffic may still flow, so only the budget ends the dial.
	fakeReach(t, 100*time.Millisecond, []tailnet.Peer{{HostName: "cabal", LastHandshake: time.Now().Add(-time.Minute), IPs: []string{"100.64.0.3"}}})
	_, err := dialPeer(context.Background(), "tcp", "100.64.0.3:7443")
	if !IsUnreachable(err) || !strings.Contains(err.Error(), "no answer within 100ms") {
		t.Fatalf("err = %v", err)
	}
}

func TestDialPeerKeepsHostWithAnotherRoute(t *testing.T) {
	// The name also resolves to a LAN address the dialer may be trying, so
	// the offline tailnet node does not settle the attempt.
	fakeReach(t, 100*time.Millisecond, []tailnet.Peer{{HostName: "cabal", IPs: []string{"100.64.0.3"}}})
	resolveHost = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("100.64.0.3"), netip.MustParseAddr("192.168.1.20")}, nil
	}
	_, err := dialPeer(context.Background(), "tcp", "cabal:7443")
	if !IsUnreachable(err) || !strings.Contains(err.Error(), "no answer within 100ms") {
		t.Fatalf("err = %v", err)
	}
	resolveHost = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("100.64.0.3")}, nil
	}
	_, err = dialPeer(context.Background(), "tcp", "cabal:7443")
	if err == nil || !strings.HasPrefix(err.Error(), "unreachable: Tailscale reports cabal offline") {
		t.Fatalf("err = %v", err)
	}
}

func TestTailnetLookupIsSharedOnlyByEarlierAttempts(t *testing.T) {
	oldAsked, oldPeers, oldErr := tailnetCache.asked, tailnetCache.peers, tailnetCache.err
	t.Cleanup(func() {
		tailnetCache.asked, tailnetCache.peers, tailnetCache.err = oldAsked, oldPeers, oldErr
	})
	asked := time.Now()
	tailnetCache.asked, tailnetCache.peers, tailnetCache.err = asked, []tailnet.Peer{{HostName: "cached"}}, nil
	if peers, err := cachedTailnetPeers(context.Background(), asked.Add(-time.Second)); err != nil || len(peers) != 1 || peers[0].HostName != "cached" {
		t.Fatalf("an attempt that began before the lookup did not share it: %v %v", peers, err)
	}
	cachedTailnetPeers(context.Background(), asked.Add(time.Nanosecond))
	if !tailnetCache.asked.After(asked) {
		t.Fatal("an attempt that began after the lookup reused it")
	}
}
