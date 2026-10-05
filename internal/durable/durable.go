// Package durable flushes files and directories to stable storage.
package durable

import "os"

// Sync flushes f, which may be a regular file or a directory.
func Sync(f *os.File) error { return syncFile(f) }
