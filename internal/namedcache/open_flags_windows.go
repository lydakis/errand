//go:build windows

package namedcache

import (
	"fmt"
	"os"

	"github.com/lydakis/errand/internal/fsowner"
)

// Windows has no equivalent open flags. os.Root still refuses links that
// leave the store.
const (
	openDirectory = 0
	openNoFollow  = 0
	openNonblock  = 0
)

// Windows directories carry no mode bits. The state directory lives in the
// user's profile, whose ACL admits only that user and the system, so private
// means owned by this user.
func checkPrivateRoot(dir string, _ os.FileInfo) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	owned, err := fsowner.OwnedByCurrentUser(f)
	if err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("named cache root must be owned by the current user")
	}
	return nil
}
