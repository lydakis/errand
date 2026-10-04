//go:build windows

package serviceruntime

import (
	"fmt"
	"os"

	"github.com/lydakis/errand/internal/fsowner"
)

const runtimeName = "errand.exe"

// Windows can't replace a running process image. The service is registered
// with the prepared runtime path, so it already runs there; a daemon started
// from elsewhere keeps running where it was started.
func execPrepared(*os.File, string) error { return nil }

// Windows reports every directory as 0777. Private means owned by this user
// under a profile directory whose ACL admits only that user and the system.
func privateDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("runtime directory must be private and not a symlink: %s", dir)
	}
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
		return fmt.Errorf("runtime directory must be owned by the current user: %s", dir)
	}
	return nil
}

// Windows records only a read-only attribute, which hard links share and
// os.Remove clears when it deletes the staging link. The content hash is the
// check that matters.
func runtimeModeOK(os.FileInfo) bool { return true }
