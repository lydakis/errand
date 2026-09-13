package snapshot

import (
	"io/fs"
	"syscall"
)

func changeStamp(info fs.FileInfo) (int64, int64, bool) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return s.Ctim.Sec, s.Ctim.Nsec, true
}
