//go:build !linux && !darwin

package unixpeer

import (
	"errors"
	"net"
)

func CurrentUID() uint32 { return ^uint32(0) }

func Credentials(_ *net.UnixConn) (Peer, error) {
	return Peer{}, errors.New("peer credentials are not supported on this platform")
}
