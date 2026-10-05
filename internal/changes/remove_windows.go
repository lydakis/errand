//go:build windows

package changes

import (
	"fmt"
	"io/fs"

	"github.com/lydakis/errand/internal/fsidentity"
)

// releaseTreeForRemoval empties the tree through its verified root and closes
// that root, because Windows cannot delete a directory while a handle to it is
// open. The caller then removes the empty root by path, which is checked to
// still be the same directory.
func releaseTreeForRemoval(access *treeAccess, rootPath string) error {
	dir, err := access.root.Open(".")
	if err != nil {
		return err
	}
	names, err := dir.Readdirnames(-1)
	if closeErr := dir.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := access.root.RemoveAll(name); err != nil {
			return err
		}
	}
	if !access.ownsRoot {
		return nil
	}
	if err := access.root.Close(); err != nil {
		return err
	}
	access.ownsRoot = false
	identity, info, err := fsidentity.Lstat(rootPath)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || identity != access.rootIdentity {
		return fmt.Errorf("retained tree root changed during removal")
	}
	return nil
}
