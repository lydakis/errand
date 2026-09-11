//go:build linux

package namedcache

import (
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

const preferTreeClone = false

func cloneTreeFile(src, dest string, info fs.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return preserveTreeFile(dest, info)
}
