//go:build !darwin && !linux

package changes

import "errors"

func cloneFile(_, _ string) error {
	return errors.New("copy-on-write cloning is unavailable")
}
