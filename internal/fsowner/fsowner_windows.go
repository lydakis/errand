//go:build windows

package fsowner

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A file counts as the current user's when its owner is the token's user or
// the token's default owner, the SID Windows assigns to everything this
// process creates. An administrator's token without UAC filtering defaults
// to BUILTIN\Administrators, so errand's own directories carry that owner.
// Accepting it grants nothing: such a token can already act as any member
// of Administrators, including taking ownership of any file.
func ownedByCurrentUser(f *os.File) (bool, error) {
	descriptor, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return false, err
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return false, err
	}
	if owner.Equals(user.User.Sid) {
		return true, nil
	}
	defaultOwner, err := tokenDefaultOwner(token)
	if err != nil {
		return false, err
	}
	return owner.Equals(defaultOwner), nil
}

// tokenDefaultOwner reads TOKEN_OWNER, which x/sys/windows has no helper for.
func tokenDefaultOwner(token windows.Token) (*windows.SID, error) {
	n := uint32(64)
	for {
		buf := make([]byte, n)
		err := windows.GetTokenInformation(token, windows.TokenOwner, &buf[0], uint32(len(buf)), &n)
		if err == nil {
			// TOKEN_OWNER is a single pointer to a SID stored later in buf.
			return (*struct{ Owner *windows.SID })(unsafe.Pointer(&buf[0])).Owner, nil
		}
		if err != windows.ERROR_INSUFFICIENT_BUFFER || n <= uint32(len(buf)) {
			return nil, err
		}
	}
}
