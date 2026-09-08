//go:build linux

package unixpeer

import (
	"net"

	"golang.org/x/sys/unix"
)

// ProcessID returns the kernel-reported PID of the connected peer.
func ProcessID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var pidErr error
	if err := raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		pidErr = err
		if err == nil {
			pid = int(cred.Pid)
		}
	}); err != nil {
		return 0, err
	}
	return pid, pidErr
}
