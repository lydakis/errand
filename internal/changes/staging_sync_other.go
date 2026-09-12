//go:build !darwin

package changes

import "os"

func syncStagedData(file *os.File) error {
	return file.Sync()
}
