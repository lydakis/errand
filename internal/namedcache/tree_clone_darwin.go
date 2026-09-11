//go:build darwin

package namedcache

import (
	"io/fs"

	"golang.org/x/sys/unix"
)

const preferTreeClone = true

// clonefile preserves mode and modification time along with the file data.
func cloneTreeFile(src, dest string, _ fs.FileInfo) error {
	return unix.Clonefile(src, dest, 0)
}
