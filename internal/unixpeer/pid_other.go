//go:build !linux && !darwin

package unixpeer

import (
	"errors"
	"net"
)

func ProcessID(_ *net.UnixConn) (int, error) {
	return 0, errors.New("peer process IDs are not supported on this platform")
}
