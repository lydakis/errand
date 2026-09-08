//go:build darwin

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
		pid, pidErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return 0, err
	}
	return pid, pidErr
}
