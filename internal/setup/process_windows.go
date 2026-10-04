//go:build windows

package setup

import (
	"golang.org/x/sys/windows"
	"strings"
)

// Task Scheduler exports user IDs as either SIDs or account names. Compare
// identities through the native account lookup without another subprocess.
func taskUserID(name string) string {
	if strings.HasPrefix(name, "S-1-") {
		return name
	}
	sid, _, _, err := windows.LookupSID("", name)
	if err != nil {
		return name
	}
	return sid.String()
}

func currentUserSID() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

func processImage(pid int) (string, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(process)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(process, 0, &buf[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:size]), nil
}
