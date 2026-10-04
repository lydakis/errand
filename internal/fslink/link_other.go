//go:build !windows

package fslink

import "os"

const NativeTypes = false

func Directory(os.FileInfo) bool { return false }

func Create(root *os.Root, target, name string, _ bool) error {
	return root.Symlink(target, name)
}

func CreatePath(target, name string, _ bool) error { return os.Symlink(target, name) }
