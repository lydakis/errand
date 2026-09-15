//go:build darwin || linux

package changes

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/lydakis/errand/internal/proto"
	"golang.org/x/sys/unix"
)

// Permission fallback for os.Root's read-open traversal. Each component is
// opened relative to a pinned descriptor without following symlinks. Keep only
// two directory handles while walking, then revalidate the parent from root.
// Ordinary readable trees continue to use the retained-parent fast path.
func searchSourceParent(root *os.Root, name string) (*os.File, string, error) {
	if name == "" || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
		return nil, "", fmt.Errorf("invalid source path %q", name)
	}
	current, err := root.Open(".")
	if err != nil {
		return nil, "", err
	}
	parts := strings.Split(name, "/")
	for _, part := range parts[:len(parts)-1] {
		fd, openErr := unix.Openat(int(current.Fd()), part, sourceSearchFlags|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		closeErr := current.Close()
		if openErr != nil {
			return nil, "", errors.Join(openErr, closeErr)
		}
		current = os.NewFile(uintptr(fd), part)
		if closeErr != nil {
			return nil, "", errors.Join(closeErr, current.Close())
		}
	}
	return current, parts[len(parts)-1], nil
}

func withSearchSourceParent(root *os.Root, name string, read func(*os.File, string) error) (err error) {
	parent, leaf, err := searchSourceParent(root, name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, parent.Close()) }()
	before, err := parent.Stat()
	if err != nil {
		return err
	}
	if err := read(parent, leaf); err != nil {
		return err
	}
	check, _, err := searchSourceParent(root, name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, check.Close()) }()
	after, err := check.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) {
		return fmt.Errorf("materialization source parent of %q changed while opening", name)
	}
	return nil
}

func openSearchSource(root *os.Root, name string, flags int) (*os.File, error) {
	var file *os.File
	err := withSearchSourceParent(root, name, func(parent *os.File, leaf string) error {
		fd, err := unix.Openat(int(parent.Fd()), leaf, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			return &os.PathError{Op: "openat", Path: name, Err: err}
		}
		file = os.NewFile(uintptr(fd), name)
		return nil
	})
	if err != nil && file != nil {
		err = errors.Join(err, file.Close())
		file = nil
	}
	return file, err
}

func openSearchSourceFile(root *os.Root, name string) (*os.File, error) {
	return openSearchSource(root, name, unix.O_RDONLY)
}

func openSearchMaterializedDirectory(root *os.Root, name string) (*os.File, error) {
	return openSearchSource(root, name, materializedDirectoryFlags|unix.O_DIRECTORY)
}

func checkSearchSource(root *os.Root, e proto.ManifestEntry, mode uint32) error {
	return withSearchSourceParent(root, e.Path, func(parent *os.File, leaf string) error {
		var st unix.Stat_t
		if err := unix.Fstatat(int(parent.Fd()), leaf, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if uint32(st.Mode)&0777 != mode {
			return fmt.Errorf("transfer source %q changed mode", e.Path)
		}
		switch e.Type {
		case proto.EntryDir:
			if st.Mode&unix.S_IFMT != unix.S_IFDIR {
				return fmt.Errorf("transfer source %q changed type", e.Path)
			}
		case proto.EntrySymlink:
			if st.Mode&unix.S_IFMT != unix.S_IFLNK {
				return fmt.Errorf("transfer source %q changed type", e.Path)
			}
			buf := make([]byte, len(e.Target)+1)
			n, err := unix.Readlinkat(int(parent.Fd()), leaf, buf)
			if err != nil {
				return err
			}
			if string(buf[:n]) != e.Target {
				return fmt.Errorf("transfer source %q changed target", e.Path)
			}
		}
		return nil
	})
}
