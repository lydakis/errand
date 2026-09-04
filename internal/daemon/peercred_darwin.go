//go:build darwin

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
		cred, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			credErr = err
			return
		}
		peer = LocalPeer{UID: cred.Uid}
		if cred.Ngroups > 0 {
			peer.GID = cred.Groups[0]
		}
	}); err != nil {
		return LocalPeer{}, err
	}
	return peer, credErr
}
