package daemon

import (
	"context"
	"net"
	"os/user"
	"strconv"
)

// LocalPeer is the kernel-attested identity of a Unix-socket caller.
type LocalPeer struct {
	UID  uint32
	GID  uint32
	User string
}

type localPeerKey struct{}

// ConnContext attaches Unix peer credentials to each connection.
func ConnContext(ctx context.Context, conn net.Conn) context.Context {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return ctx
	}
	peer, err := peerCredentials(unixConn)
	if err != nil {
		return ctx
	}
	if u, lookupErr := user.LookupId(strconv.FormatUint(uint64(peer.UID), 10)); lookupErr == nil {
		peer.User = u.Username
	}
	return context.WithValue(ctx, localPeerKey{}, peer)
}

func localPeerFromContext(ctx context.Context) (LocalPeer, bool) {
	peer, ok := ctx.Value(localPeerKey{}).(LocalPeer)
	return peer, ok
}
