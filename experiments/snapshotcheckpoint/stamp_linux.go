package snapshotcheckpoint

import (
	"fmt"
	"golang.org/x/sys/unix"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

func fingerprint(info fs.FileInfo) (stamp, error) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return stamp{}, fmt.Errorf("unsupported stat evidence")
	}
	return stamp{Device: uint64(s.Dev), Inode: s.Ino, Size: info.Size(), Mode: uint32(info.Mode()),
		ModifiedSec: s.Mtim.Sec, ModifiedNS: s.Mtim.Nsec,
		ChangedSec: s.Ctim.Sec, ChangedNS: s.Ctim.Nsec}, nil
}

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
