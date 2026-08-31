//go:build linux

package main

import (
	"syscall"
	"unsafe"
)

func fileTerminalColumns(fd uintptr) int {
	var size struct {
		rows uint16
		cols uint16
		x    uint16
		y    uint16
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, 0x5413, uintptr(unsafe.Pointer(&size)))
	if errno != 0 {
		return 0
	}
	return int(size.cols)
}
