//go:build windows

package unixpeer

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// Winsock reports WSAECONNREFUSED, not the syscall package's ECONNREFUSED.
func connectionRefused(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED) || errors.Is(err, syscall.ECONNREFUSED)
}
