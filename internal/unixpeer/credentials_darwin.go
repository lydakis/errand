//go:build darwin

package unixpeer

import (
	"net"

	"golang.org/x/sys/unix"
)

func CurrentUID() uint32 { return uint32(unix.Geteuid()) }

func Credentials(conn *net.UnixConn) (Peer, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return Peer{}, err
	}
	var peer Peer
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			credErr = err
			return
		}
		peer = Peer{UID: cred.Uid}
		if cred.Ngroups > 0 {
			peer.GID = cred.Groups[0]
		}
	}); err != nil {
		return Peer{}, err
	}
	return peer, credErr
}
