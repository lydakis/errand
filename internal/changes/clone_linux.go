//go:build linux

package changes

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func cloneFileInto(source *os.File, tree *os.Root, name string) (*os.File, error) {
	file, err := tree.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.IoctlFileClone(int(file.Fd()), int(source.Fd())); err != nil {
		return nil, errors.Join(err, file.Close(), tree.Remove(name))
	}
	return file, nil
}
