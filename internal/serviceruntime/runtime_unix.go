//go:build unix

package serviceruntime

import (
	"fmt"
	"os"
	"syscall"
)

const runtimeName = "errand"

func execPrepared(source *os.File, target string) error {
	// Keep the opened installation's identity: package cleanup may have
	// removed its pathname since we opened it, even after publication.
	original, err := source.Stat()
	if err != nil {
		return err
	}
	runtime, err := os.Stat(target)
	if err != nil {
		return err
	}
	if os.SameFile(original, runtime) {
		return nil
	}
	return syscall.Exec(target, append([]string{target}, os.Args[1:]...), os.Environ())
}

func privateDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("runtime directory must be private and not a symlink: %s", dir)
	}
	return nil
}

func runtimeModeOK(info os.FileInfo) bool { return info.Mode().Perm() == 0500 }
