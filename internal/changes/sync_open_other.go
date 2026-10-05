//go:build !windows

package changes

import "os"

func openApplyBackupFile(root *os.Root, name string, _ os.FileInfo) (*os.File, error) {
	return openSearchSourceFile(root, name)
}

func openRetainedSyncFile(root *os.Root, name string, _ os.FileInfo) (*os.File, error) {
	return root.Open(name)
}
