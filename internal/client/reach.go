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
// Tailscale node, tailscaled already knows whether that node is offline;
// asking it turns a timeout into an immediate, named answer.
var (
	peerConnectTimeout = 3 * time.Second
	tailnetCheckAfter  = 300 * time.Millisecond
	dialTCP            = (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext
	tailnetPeers       = cachedTailnetPeers
)

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
	check := time.AfterFunc(tailnetCheckAfter, func() {
		if err := tailnetOffline(dialCtx, addr); err != nil {
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

// tailnetOffline returns an UnreachableError when addr belongs to a node that
// tailscaled reports offline. Any doubt (not a tailnet address, no
// tailscaled, an unknown node) leaves the connection attempt alone.
func tailnetOffline(ctx context.Context, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else if ips, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host); err != nil {
		return nil
	}
	wanted := map[netip.Addr]bool{}
	for _, ip := range ips {
		ip = ip.Unmap()
		for _, prefix := range tailnetPrefixes {
			if prefix.Contains(ip) {
				wanted[ip] = true
			}
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	peers, err := tailnetPeers(ctx)
	if err != nil {
		return nil
	}
	for _, peer := range peers {
		for _, raw := range peer.IPs {
			ip, err := netip.ParseAddr(raw)
			if err != nil || !wanted[ip.Unmap()] {
				continue
			}
			if peer.Online {
				return nil
			}
			name := peer.HostName
			if name == "" {
				name = peer.DNSName
			}
			reason := "Tailscale reports " + name + " offline"
			if !peer.LastSeen.IsZero() {
				reason += ", last seen " + agoText(time.Since(peer.LastSeen))
			}
			return &UnreachableError{Addr: addr, Reason: reason}
		}
	}
	return nil
}

var tailnetCache struct {
	sync.Mutex
	done  bool
	peers []tailnet.Peer
	err   error
}

// cachedTailnetPeers asks tailscaled once per process. Only connections that
// are already slow get here, so a healthy fleet never pays for the lookup.
func cachedTailnetPeers(ctx context.Context) ([]tailnet.Peer, error) {
	tailnetCache.Lock()
	defer tailnetCache.Unlock()
	if !tailnetCache.done {
		provider, err := tailnet.Discover("", "")
		if err == nil {
			tailnetCache.peers, err = provider.Peers(ctx)
		}
		// A lookup cut short by this dial's deadline may succeed for the next.
		if ctx.Err() == nil {
			tailnetCache.done, tailnetCache.err = true, err
		}
		if err != nil {
			return nil, err
		}
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
