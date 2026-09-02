//go:build darwin

package changes

import "golang.org/x/sys/unix"

func cloneFile(src, dest string) error {
	return unix.Clonefile(src, dest, 0)
}
