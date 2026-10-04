//go:build unix

package fsowner

import (
	"fmt"
	"os"
	"syscall"
)

func ownedByCurrentUser(f *os.File) (bool, error) {
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("ownership is unavailable for %q", f.Name())
	}
	return int(stat.Uid) == os.Geteuid(), nil
}
