//go:build !windows

package setup

import (
	"errors"
	"runtime"
)

func currentUserSID() (string, error) {
	return "", errors.New("user SIDs exist only on Windows, not " + runtime.GOOS)
}

func processImage(int) (string, error) {
	return "", errors.New("process images are read only on Windows, not " + runtime.GOOS)
}
