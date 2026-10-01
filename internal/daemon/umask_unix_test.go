//go:build unix

package daemon

import (
	"syscall"
	"testing"
)

func setUmask(_ *testing.T, mask int) (restore func()) {
	old := syscall.Umask(mask)
	return func() { syscall.Umask(old) }
}
