//go:build windows

package changes

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const fileRenameInformationEx = 65

// fileRenameInformation is FILE_RENAME_INFORMATION_EX. With Flags zero it is
// also a valid FILE_RENAME_INFORMATION with ReplaceIfExists false.
type fileRenameInformation struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

// renameNoReplace renames relative to open directory handles, like renameat2
// with RENAME_NOREPLACE, so a parent swapped by path cannot redirect it.
func renameNoReplace(fromDir *os.File, from string, toDir *os.File, to string) error {
	source, err := openForRename(fromDir, from)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	defer windows.CloseHandle(source)

	name, err := windows.UTF16FromString(to)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	name = name[:len(name)-1]
	size := int(unsafe.Offsetof(fileRenameInformation{}.FileName)) + len(name)*2
	buffer := make([]byte, max(size, int(unsafe.Sizeof(fileRenameInformation{}))))
	info := (*fileRenameInformation)(unsafe.Pointer(&buffer[0]))
	info.RootDirectory = windows.Handle(toDir.Fd())
	info.FileNameLength = uint32(len(name) * 2)
	copy(unsafe.Slice(&info.FileName[0], len(name)), name)

	// POSIX semantics let the rename proceed while other handles are open. File
	// systems without FILE_RENAME_INFORMATION_EX get the classic request.
	info.Flags = windows.FILE_RENAME_POSIX_SEMANTICS
	err = windows.NtSetInformationFile(source, new(windows.IO_STATUS_BLOCK), &buffer[0], uint32(len(buffer)), fileRenameInformationEx)
	if errors.Is(err, windows.STATUS_INVALID_INFO_CLASS) || errors.Is(err, windows.STATUS_INVALID_PARAMETER) || errors.Is(err, windows.STATUS_NOT_SUPPORTED) {
		info.Flags = 0
		err = windows.NtSetInformationFile(source, new(windows.IO_STATUS_BLOCK), &buffer[0], uint32(len(buffer)), windows.FileRenameInformation)
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: ntError(err)}
	}
	return nil
}

func openForRename(dir *os.File, name string) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(dir.Fd()), ObjectName: objectName}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle,
		windows.SYNCHRONIZE|windows.DELETE,
		&attributes,
		new(windows.IO_STATUS_BLOCK),
		nil,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	if err != nil {
		return 0, ntError(err)
	}
	return handle, nil
}

func ntError(err error) error {
	if status, ok := err.(windows.NTStatus); ok {
		return status.Errno()
	}
	return err
}
