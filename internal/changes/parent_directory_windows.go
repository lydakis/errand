//go:build windows

package changes

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Windows has no ".." entry to open relative to a handle. Resolve the opened
// directory's final path instead, so the walk still follows the real object
// rather than the path it was reached by.
func openParentDirectory(dir *os.File) (*os.File, error) {
	path, err := finalPath(windows.Handle(dir.Fd()))
	if err != nil {
		return nil, err
	}
	return os.Open(filepath.Dir(path))
}

func finalPath(handle windows.Handle) (string, error) {
	buffer := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0) // FILE_NAME_NORMALIZED | VOLUME_NAME_DOS
		if err != nil {
			return "", err
		}
		if int(n) < len(buffer) {
			return windows.UTF16ToString(buffer[:n]), nil
		}
		buffer = make([]uint16, n)
	}
}
