//go:build linux

package client

import (
	"os"
	"syscall"
	"time"
	"unsafe"
)

func isTerminalFile(f *os.File) bool {
	if f == nil {
		return false
	}
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(syscall.TCGETS),
		uintptr(unsafe.Pointer(&termios)),
	)
	if errno != 0 {
		return false
	}
	var foregroundPGRP int32
	_, _, errno = syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(syscall.TIOCGPGRP),
		uintptr(unsafe.Pointer(&foregroundPGRP)),
	)
	return errno == 0 && int(foregroundPGRP) == syscall.Getpgrp()
}

func waitTerminalReadable(f *os.File, timeout time.Duration) (bool, error) {
	return selectTerminalReadable(f, timeout)
}

func selectTerminalReadable(f *os.File, timeout time.Duration) (bool, error) {
	fd := int(f.Fd())
	var readSet syscall.FdSet
	if fd < 0 || fd >= int(unsafe.Sizeof(readSet))*8 {
		return false, syscall.EINVAL
	}
	wordBits := int(unsafe.Sizeof(uintptr(0))) * 8
	words := unsafe.Slice(
		(*uintptr)(unsafe.Pointer(&readSet)),
		int(unsafe.Sizeof(readSet)/unsafe.Sizeof(uintptr(0))),
	)
	words[fd/wordBits] |= uintptr(1) << uint(fd%wordBits)
	tv := syscall.NsecToTimeval(timeout.Nanoseconds())
	n, err := syscall.Select(fd+1, &readSet, nil, nil, &tv)
	if err == syscall.EINTR {
		return false, nil
	}
	return n > 0, err
}
