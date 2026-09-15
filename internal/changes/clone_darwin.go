//go:build darwin

package changes

import (
	"errors"
	"os"
	"path"

	"golang.org/x/sys/unix"
)

func cloneFileInto(source *os.File, tree *os.Root, name string) (*os.File, error) {
	parent, err := tree.Open(path.Dir(name))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	if err := unix.Fclonefileat(int(source.Fd()), int(parent.Fd()), path.Base(name), 0); err != nil {
		return nil, err
	}
	file, err := tree.Open(name)
	if err != nil {
		return nil, errors.Join(err, tree.Remove(name))
	}
	return file, nil
}
