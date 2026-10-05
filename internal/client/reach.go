package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lydakis/errand/internal/tailnet"
)

// Reaching a peer is decided once, here, for every command: a peer that
// cannot be connected to fails in the connect step instead of holding the
// command for its whole request budget.
//
// A live runner accepts a TCP connection within a round trip or two, even
// through a Tailscale relay, so the connect step gets its own short budget
// while the request that follows keeps its full one. When a connection is
// still pending shortly after it starts and its address belongs to a
// Tailscale node, tailscaled may already know that node is gone; asking it
// turns a timeout into an immediate, named answer.
//
// The answer only short-circuits an attempt it settles completely; see
// tailnetGone. Tailscale's Online flag is control-plane state (a node that
// lost its coordination server can still carry traffic, for example over a
// LAN), so it counts only together with the absence of a recent WireGuard
// handshake, which any working data path would have.
var (
	peerConnectTimeout = 3 * time.Second
	tailnetCheckAfter  = 300 * time.Millisecond
	dialTCP            = (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext
	tailnetPeers       = tailnetLookup(cachedTailnetPeers)
	resolveHost        = net.DefaultResolver.LookupNetIP
)

// wireGuardSessionLife is WireGuard's Reject-After-Time: a peer with no
// handshake for this long has no usable session.
const wireGuardSessionLife = 3 * time.Minute

// UnreachableError reports that nothing could be connected to at a peer's
// address, so no request was made.
type UnreachableError struct {
	Addr   string
	Reason string
	Err    error
}

func (e *UnreachableError) Error() string { return "unreachable: " + e.Reason }
func (e *UnreachableError) Unwrap() error { return e.Err }

// IsUnreachable reports whether err means the peer could not be connected to.
func IsUnreachable(err error) bool {
	var unreachable *UnreachableError
	return errors.As(err, &unreachable)
}

func dialPeer(ctx context.Context, network, addr string) (net.Conn, error) {
	dialCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	dialCtx, cancelBudget := context.WithTimeout(dialCtx, peerConnectTimeout)
	defer cancelBudget()
	a := newAttempt()
	check := time.AfterFunc(tailnetCheckAfter, func() {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return
		}
		if err := a.tailnetGone(dialCtx, host); err != nil {
			cancel(err)
		}
	})
	conn, err := dialTCP(dialCtx, network, addr)
	check.Stop()
	if err == nil {
		return conn, nil
	}
	if ctx.Err() != nil {
		return nil, err // the caller gave up; its own deadline explains why
	}
	var offline *UnreachableError
	if errors.As(context.Cause(dialCtx), &offline) {
		return nil, offline
	}
	return nil, &UnreachableError{Addr: addr, Reason: dialFailure(err), Err: err}
}

func dialFailure(err error) string {
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr):
		return "cannot resolve " + dnsErr.Name
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return fmt.Sprintf("no answer within %s", peerConnectTimeout)
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused; is errand running there?"
	case errors.Is(err, syscall.EHOSTUNREACH):
		return "no route to host"
	case errors.Is(err, syscall.ENETUNREACH):
		return "network is unreachable"
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		msg = msg[i+2:]
	}
	return msg
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

var tailnetPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// attempt is one connection attempt, with the lookups it may use captured
// when it began.
type attempt struct {
	began   time.Time
	lookup  tailnetLookup
	resolve func(ctx context.Context, network, host string) ([]netip.Addr, error)
}

func newAttempt() attempt {
	return attempt{began: time.Now(), lookup: tailnetPeers, resolve: resolveHost}
}

// tailnetGone returns an UnreachableError only when tailscaled's answer
// settles the whole attempt: every address host resolves to belongs to a
// node that is offline and has no WireGuard handshake from this machine
// within a session lifetime, as observed after the attempt began. Anything
// less certain (an address outside the tailnet, an unknown node, a recent
// handshake, no tailscaled) leaves the attempt to its own connect budget.
func (a attempt) tailnetGone(ctx context.Context, host string) error {
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		ips = []netip.Addr{ip}
	} else if ips, err = a.resolve(ctx, "ip", host); err != nil || len(ips) == 0 {
		return nil
	}
	for _, ip := range ips {
		if !inTailnet(ip.Unmap()) {
			return nil
		}
	}
	peers, err := a.lookup(ctx, a.began)
	if err != nil {
		return nil
	}
	var gone *tailnet.Peer
	for _, ip := range ips {
		peer := tailnetPeerFor(peers, ip.Unmap())
		if peer == nil || peer.Online || time.Since(peer.LastHandshake) < wireGuardSessionLife {
			return nil
		}
		gone = peer
	}
	name := gone.HostName
	if name == "" {
		name = gone.DNSName
	}
	reason := "Tailscale reports " + name + " offline"
	if !gone.LastSeen.IsZero() {
		reason += ", last seen " + agoText(time.Since(gone.LastSeen))
	}
	return &UnreachableError{Addr: host, Reason: reason}
}

func inTailnet(ip netip.Addr) bool {
	for _, prefix := range tailnetPrefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

func tailnetPeerFor(peers []tailnet.Peer, ip netip.Addr) *tailnet.Peer {
	for i := range peers {
		for _, raw := range peers[i].IPs {
			if candidate, err := netip.ParseAddr(raw); err == nil && candidate.Unmap() == ip {
				return &peers[i]
			}
		}
	}
	return nil
}

// tailnetLookup returns tailscaled's view of the tailnet as observed no
// earlier than notBefore.
type tailnetLookup func(ctx context.Context, notBefore time.Time) ([]tailnet.Peer, error)

var tailnetCache struct {
	sync.Mutex
	asked time.Time // when the cached answer was requested
	peers []tailnet.Peer
	err   error
}

// cachedTailnetPeers shares one tailscaled lookup among the attempts that
// began before it was made, such as one fan-out's slow connections; an
// attempt that began later gets a new one, so a handshake it made is seen.
// Only connections that are already slow get here, so a healthy fleet never
// pays for the lookup.
func cachedTailnetPeers(ctx context.Context, notBefore time.Time) ([]tailnet.Peer, error) {
	tailnetCache.Lock()
	defer tailnetCache.Unlock()
	if tailnetCache.asked.IsZero() || tailnetCache.asked.Before(notBefore) {
		asked := time.Now()
		provider, err := tailnet.Discover("", "")
		var peers []tailnet.Peer
		if err == nil {
			peers, err = provider.Peers(ctx)
		}
		// A lookup cut short by this dial's deadline may succeed for the next.
		if ctx.Err() != nil {
			return peers, err
		}
		tailnetCache.asked, tailnetCache.peers, tailnetCache.err = asked, peers, err
	}
	return tailnetCache.peers, tailnetCache.err
}

func agoText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}
