// Package fsidentity records stable Unix filesystem object identities.
package fsidentity

import (
	"fmt"
	"os"
	"syscall"
)

// Identity distinguishes one filesystem object from another at the same path.
type Identity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

func FromInfo(info os.FileInfo) (Identity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Identity{}, fmt.Errorf("filesystem identity is unavailable for %q", info.Name())
	}
	return Identity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}

func Lstat(path string) (Identity, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Identity{}, nil, err
	}
	identity, err := FromInfo(info)
	return identity, info, err
}

func (i Identity) IsZero() bool {
	return i == (Identity{})
}
