//go:build unix

package fsidentity

import (
	"fmt"
	"os"
	"syscall"
)

func FromInfo(info os.FileInfo) (Identity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Identity{}, fmt.Errorf("filesystem identity is unavailable for %q", info.Name())
	}
	return Identity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}
