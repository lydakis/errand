package snapshot

import (
	"fmt"
	"io/fs"
	"syscall"
)

func changeStamp(info fs.FileInfo) (int64, int64, bool) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return s.Ctimespec.Sec, s.Ctimespec.Nsec, true
}

func Fingerprint(info fs.FileInfo) (ObservationStamp, error) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ObservationStamp{}, fmt.Errorf("unsupported stat evidence")
	}
	return ObservationStamp{Device: uint64(s.Dev), Inode: s.Ino, Size: info.Size(), Mode: uint32(info.Mode()),
		ModifiedSec: s.Mtimespec.Sec, ModifiedNS: s.Mtimespec.Nsec,
		ChangedSec: s.Ctimespec.Sec, ChangedNS: s.Ctimespec.Nsec,
		BornSec: s.Birthtimespec.Sec, BornNS: s.Birthtimespec.Nsec}, nil
}
