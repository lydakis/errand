//go:build windows

package fsowner

import (
	"os"

	"golang.org/x/sys/windows"
)

func ownedByCurrentUser(f *os.File) (bool, error) {
	descriptor, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return false, err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false, err
	}
	return owner.Equals(user.User.Sid), nil
}
