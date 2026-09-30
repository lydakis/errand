//go:build unix

package namedcache

import "syscall"

const (
	openDirectory = syscall.O_DIRECTORY
	openNoFollow  = syscall.O_NOFOLLOW
	openNonblock  = syscall.O_NONBLOCK
)
