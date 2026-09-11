//go:build !darwin && !linux

package namedcache

import (
	"errors"
	"io/fs"
)

const preferTreeClone = false

func cloneTreeFile(_, _ string, _ fs.FileInfo) error {
	return errors.New("copy-on-write cloning is unavailable")
}
