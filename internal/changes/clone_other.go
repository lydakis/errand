//go:build !darwin && !linux

package changes

import (
	"errors"
	"os"
)

func cloneFileInto(_ *os.File, _ *os.Root, _ string) (*os.File, error) {
	return nil, errors.New("copy-on-write cloning is unavailable")
}
