//go:build windows

package fslink

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const NativeTypes = true

func Directory(info os.FileInfo) bool {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && data.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
}

func CreatePath(target, name string, directory bool) error {
	root, err := os.OpenRoot(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer root.Close()
	return Create(root, target, filepath.Base(name), directory)
}

// Create uses the same handle-relative NT operations as os.Root.Symlink, with
// an explicit directory flag. A path-based CreateSymbolicLink call would lose
// the root's protection against renamed or replaced parents.
func Create(root *os.Root, target, name string, directory bool) error {
	if !filepath.IsLocal(name) || filepath.VolumeName(target) != "" || filepath.IsAbs(target) || target == "" {
		return &os.LinkError{Op: "symlink", Old: target, New: name, Err: windows.ERROR_INVALID_PARAMETER}
	}
	parent, err := root.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer parent.Close()
	create := func() error {
		return createRelativeLink(windows.Handle(parent.Fd()), filepath.Base(name), filepath.FromSlash(target), directory)
	}
	err = create()
	if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
		// Developer Mode needs no privilege adjustment. Elevated callers may
		// instead hold a disabled symlink privilege, like CreateSymbolicLinkW.
		err = withSymlinkPrivilege(create)
	}
	if err != nil {
		return &os.LinkError{Op: "symlink", Old: target, New: name, Err: err}
	}
	return nil
}

func createRelativeLink(parent windows.Handle, name, target string, directory bool) error {
	text, err := windows.UTF16FromString(target)
	if err != nil {
		return err
	}
	text = text[:len(text)-1]
	const header = 20 // REPARSE_DATA_BUFFER with a SymbolicLinkReparseBuffer
	if header+2*len(text) > windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE {
		return windows.ERROR_FILENAME_EXCED_RANGE
	}
	data := make([]byte, header+2*len(text))
	binary.LittleEndian.PutUint32(data, windows.IO_REPARSE_TAG_SYMLINK)
	binary.LittleEndian.PutUint16(data[4:], uint16(len(data)-8))
	binary.LittleEndian.PutUint16(data[10:], uint16(2*len(text)))
	binary.LittleEndian.PutUint16(data[14:], uint16(2*len(text)))
	binary.LittleEndian.PutUint32(data[16:], 1) // SYMLINK_FLAG_RELATIVE
	for i, c := range text {
		binary.LittleEndian.PutUint16(data[header+2*i:], c)
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return err
	}
	attributes := windows.OBJECT_ATTRIBUTES{RootDirectory: parent, ObjectName: objectName}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	options := uint32(windows.FILE_NON_DIRECTORY_FILE)
	if directory {
		options = windows.FILE_DIRECTORY_FILE
	}
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, windows.SYNCHRONIZE|windows.FILE_WRITE_ATTRIBUTES|windows.DELETE,
		&attributes, new(windows.IO_STATUS_BLOCK), nil, windows.FILE_ATTRIBUTE_NORMAL, 0, windows.FILE_CREATE,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT|options, 0, 0)
	if err != nil {
		return ntError(err)
	}
	defer windows.CloseHandle(handle)
	var returned uint32
	if err := windows.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT, &data[0], uint32(len(data)), nil, 0, &returned, nil); err != nil {
		// Remove the failed placeholder through its pinned handle.
		remove := byte(1)
		cleanup := windows.NtSetInformationFile(handle, new(windows.IO_STATUS_BLOCK), &remove, 1, windows.FileDispositionInformation)
		return errors.Join(err, ntError(cleanup))
	}
	return nil
}

func ntError(err error) error {
	if status, ok := err.(windows.NTStatus); ok {
		return status.Errno()
	}
	return err
}

func withSymlinkPrivilege(create func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.ImpersonateSelf(windows.SecurityImpersonation); err != nil {
		return create()
	}
	defer windows.RevertToSelf()
	var token windows.Token
	if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY|windows.TOKEN_ADJUST_PRIVILEGES, false, &token); err != nil {
		return create()
	}
	defer token.Close()
	name, _ := windows.UTF16PtrFromString("SeCreateSymbolicLinkPrivilege")
	privilege := windows.Tokenprivileges{PrivilegeCount: 1}
	privilege.Privileges[0].Attributes = windows.SE_PRIVILEGE_ENABLED
	if err := windows.LookupPrivilegeValue(nil, name, &privilege.Privileges[0].Luid); err == nil {
		_ = windows.AdjustTokenPrivileges(token, false, &privilege, 0, nil, nil)
	}
	return create()
}
