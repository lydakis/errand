package snapshotcheckpoint

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"strings"
)

func bootIdentity() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b)), err
}

func filesystemIdentity(root string) (string, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(root, &s); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%v", s.Type, s.Fsid), nil
}
