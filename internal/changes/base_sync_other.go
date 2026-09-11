//go:build !darwin

package changes

import "os"

func syncCapturedData(file *os.File) error {
	return file.Sync()
}
