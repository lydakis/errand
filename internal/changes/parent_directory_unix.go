//go:build unix

package changes

import (
	"os"

	"golang.org/x/sys/unix"
)

func openParentDirectory(dir *os.File) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), "..", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "transfer state ancestor"), nil
}
