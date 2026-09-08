// Package unixpeer authenticates Unix socket peers using kernel credentials.
package unixpeer

import (
	"context"
	"fmt"
	"net"
)

// Peer is the effective identity of the process at the other end of a socket.
type Peer struct{ UID, GID uint32 }

// Dial connects only to a server running as the expected effective UID.
// Credentials are checked on the connected socket before any request is sent,
// so replacing a socket pathname cannot redirect data to another local user.
func Dial(ctx context.Context, socket string, uid uint32) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	peer, err := Credentials(conn.(*net.UnixConn))
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("cannot authenticate Unix socket server: %w", err)
	}
	if peer.UID != uid {
		conn.Close()
		return nil, fmt.Errorf("Unix socket server UID %d does not match current UID %d", peer.UID, uid)
	}
	return conn, nil
}
