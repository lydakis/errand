// Package fsidentity records stable filesystem object identities.
package fsidentity

import "os"

// Identity distinguishes one filesystem object from another at the same path.
type Identity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
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
