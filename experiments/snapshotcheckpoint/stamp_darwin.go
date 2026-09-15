package snapshotcheckpoint

import (
	"fmt"
	"golang.org/x/sys/unix"
	"io/fs"
	"syscall"
)

func fingerprint(info fs.FileInfo) (stamp, error) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return stamp{}, fmt.Errorf("unsupported stat evidence")
	}
	return stamp{Device: uint64(s.Dev), Inode: s.Ino, Size: info.Size(), Mode: uint32(info.Mode()),
		ModifiedSec: s.Mtimespec.Sec, ModifiedNS: s.Mtimespec.Nsec,
		ChangedSec: s.Ctimespec.Sec, ChangedNS: s.Ctimespec.Nsec,
		BornSec: s.Birthtimespec.Sec, BornNS: s.Birthtimespec.Nsec}, nil
}

func bootIdentity() (string, error) { return unix.Sysctl("kern.bootsessionuuid") }

func filesystemIdentity(root string) (string, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(root, &s); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%v", s.Type, s.Fsid), nil
}
