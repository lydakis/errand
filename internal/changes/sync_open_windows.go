//go:build windows

package changes

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

var procReOpenFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

func openApplyBackupFile(root *os.Root, name string, info os.FileInfo) (*os.File, error) {
	return openRetainedSyncFile(root, name, info)
}

// FlushFileBuffers needs write access. These paths are private staging or
// renamed backups; callers verify the opened identity before flushing. Keep
// the ordinary writable-file path to a single open and preserve read-only
// attributes when obtaining a writable handle to an existing read-only file.
func openRetainedSyncFile(root *os.Root, name string, info os.FileInfo) (*os.File, error) {
	if info.IsDir() {
		return root.Open(name)
	}
	if info.Mode().Perm()&0o200 != 0 {
		return root.OpenFile(name, os.O_RDWR, 0)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Mode() != info.Mode() {
		return nil, errors.Join(fmt.Errorf("retained path %q changed while opening for sync", name), err)
	}
	attributes, err := reopenSyncFile(file, windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES)
	if err != nil {
		return nil, err
	}
	defer attributes.Close()
	mode := info.Mode().Perm()
	if err := attributes.Chmod(mode | 0o200); err != nil {
		return nil, err
	}
	writable, openErr := reopenSyncFile(file, windows.GENERIC_READ|windows.GENERIC_WRITE)
	// Restore through the pinned metadata handle even if reopening fails. The
	// writable handle keeps its granted access after the attribute is restored.
	restoreErr := attributes.Chmod(mode)
	if err := errors.Join(openErr, restoreErr); err != nil {
		if writable != nil {
			err = errors.Join(err, writable.Close())
		}
		return nil, err
	}
	return writable, nil
}

func reopenSyncFile(file *os.File, access uint32) (*os.File, error) {
	handle, _, err := procReOpenFile.Call(file.Fd(), uintptr(access),
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if windows.Handle(handle) == windows.InvalidHandle {
		return nil, &os.PathError{Op: "reopen for sync", Path: file.Name(), Err: err}
	}
	return os.NewFile(handle, file.Name()), nil
}
