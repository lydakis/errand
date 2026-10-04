//go:build windows

package durable

import "os"

// Windows can't flush a directory handle opened for reading. NTFS journals
// directory entries itself, so a completed rename needs no further barrier.
func syncFile(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}
	return f.Sync()
}
