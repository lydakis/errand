//go:build !linux && !darwin

package daemon

import (
	"errors"
	"net"
)

func currentUID() uint32 { return ^uint32(0) }

func peerCredentials(_ *net.UnixConn) (LocalPeer, error) {
	return LocalPeer{}, errors.New("peer credentials are not supported on this platform")
}
