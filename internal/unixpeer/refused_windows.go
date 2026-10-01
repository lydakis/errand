//go:build windows

package unixpeer

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// Winsock reports WSAECONNREFUSED, not the syscall package's ECONNREFUSED.
// Dialing a socket whose directory does not exist yet reports WSAENETDOWN
// instead of a missing file; nothing listens there either.
func connectionRefused(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, windows.WSAENETDOWN)
}
