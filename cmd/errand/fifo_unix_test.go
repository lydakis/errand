//go:build unix

package main

import (
	"syscall"
	"testing"
)

// requireFIFOs skips tests that need named pipes in the file system.
func requireFIFOs(*testing.T) {}

func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }
