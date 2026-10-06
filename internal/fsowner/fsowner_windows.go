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

// Access rights that let a trustee read a file's data, directly or by
// changing who may.
const readRights = windows.FILE_READ_DATA | windows.GENERIC_READ | windows.GENERIC_ALL | windows.WRITE_DAC | windows.WRITE_OWNER

// ACE types besides ACCESS_ALLOWED_ACE_TYPE that grant access.
// ACCESS_ALLOWED_CALLBACK_ACE has its layout; the object types do not.
const (
	accessAllowedObjectACE         = 0x5
	accessAllowedCallbackACE       = 0x9
	accessAllowedCallbackObjectACE = 0xb
)

// private walks f's DACL. Entries that deny access are skipped, and an
// entry granting access in a form this does not parse counts as letting
// others read. Either can only make a file look less private than it is.
func private(f *os.File) (bool, error) {
	descriptor, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return false, err
	}
	dacl, _, err := descriptor.DACL()
	if err == windows.ERROR_OBJECT_NOT_FOUND || err == nil && dacl == nil {
		return false, nil // no DACL, or a null one, lets everyone in
	}
	if err != nil {
		return false, err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false, err
	}
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false, err
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue // applies only to what a directory will hold
		}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE, accessAllowedCallbackACE:
		case accessAllowedObjectACE, accessAllowedCallbackObjectACE:
			return false, nil
		default:
			continue
		}
		if ace.Mask&readRights == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.Equals(owner) || sid.Equals(user.User.Sid) ||
			sid.IsWellKnown(windows.WinCreatorOwnerRightsSid) ||
			sid.IsWellKnown(windows.WinLocalSystemSid) ||
			sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			continue
		}
		return false, nil
	}
	return true, nil
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
