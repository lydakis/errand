//go:build linux

package changes

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameNoReplace(fromDir *os.File, from string, toDir *os.File, to string) error {
	return unix.Renameat2(int(fromDir.Fd()), from, int(toDir.Fd()), to, unix.RENAME_NOREPLACE)
}
