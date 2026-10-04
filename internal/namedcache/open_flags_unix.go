//go:build unix

package namedcache

import (
	"fmt"
	"os"
	"syscall"
)

const (
	openDirectory = syscall.O_DIRECTORY
	openNoFollow  = syscall.O_NOFOLLOW
	openNonblock  = syscall.O_NONBLOCK
)

func checkPrivateRoot(_ string, info os.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("named cache root must be private (mode 0700)")
	}
	return nil
}
