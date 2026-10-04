//go:build unix

package durable

import "os"

func syncFile(f *os.File) error { return f.Sync() }
