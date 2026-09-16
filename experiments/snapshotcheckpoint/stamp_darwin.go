package snapshotcheckpoint

import (
	"fmt"
	"golang.org/x/sys/unix"
)

func bootIdentity() (string, error) { return unix.Sysctl("kern.bootsessionuuid") }

func filesystemIdentity(root string) (string, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(root, &s); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%v", s.Type, s.Fsid), nil
}
