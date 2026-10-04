//go:build windows

package daemon

import "testing"

func setUmask(t *testing.T, _ int) (restore func()) {
	t.Skip("Windows has no umask")
	return nil
}
