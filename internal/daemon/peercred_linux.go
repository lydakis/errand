//go:build linux

package daemon

import (
	"net"

	"golang.org/x/sys/unix"
)

func currentUID() uint32 { return uint32(unix.Geteuid()) }

func peerCredentials(conn *net.UnixConn) (LocalPeer, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return LocalPeer{}, err
	}
	var peer LocalPeer
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			credErr = err
			return
		}
		peer = LocalPeer{UID: cred.Uid, GID: cred.Gid}
	}); err != nil {
		return LocalPeer{}, err
	}
	return peer, credErr
}
