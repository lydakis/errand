//go:build windows

package unixpeer

import (
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows has no numeric uids. Peers are compared by their token's user SID,
// and UID only records the result: SameUser when the peer runs as the same
// account as this process, OtherUser otherwise.
const (
	SameUser  uint32 = 0
	OtherUser uint32 = 1
)

// sioAFUnixGetPeerPID is SIO_AF_UNIX_GETPEERPID from afunix.h.
const sioAFUnixGetPeerPID = 0x58000100

func CurrentUID() uint32 { return SameUser }

func Credentials(conn *net.UnixConn) (Peer, error) {
	pid, err := ProcessID(conn)
	if err != nil {
		return Peer{}, err
	}
	sid, account, err := processUser(uint32(pid))
	if err != nil {
		return Peer{}, fmt.Errorf("reading peer process %d user: %w", pid, err)
	}
	self, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return Peer{}, err
	}
	peer := Peer{UID: OtherUser, User: account}
	if sid.Equals(self.User.Sid) {
		peer.UID = SameUser
	}
	return peer, nil
}

// ProcessID returns the PID Windows reports for the connected peer.
func ProcessID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid uint32
	var pidErr error
	if err := raw.Control(func(fd uintptr) {
		var returned uint32
		pidErr = windows.WSAIoctl(windows.Handle(fd), sioAFUnixGetPeerPID, nil, 0,
			(*byte)(unsafe.Pointer(&pid)), uint32(unsafe.Sizeof(pid)), &returned, nil, 0)
	}); err != nil {
		return 0, err
	}
	if pidErr != nil {
		return 0, pidErr
	}
	if pid == 0 {
		return 0, fmt.Errorf("peer process ID is unavailable")
	}
	return int(pid), nil
}

func processUser(pid uint32) (*windows.SID, string, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return nil, "", err
	}
	defer windows.CloseHandle(process)
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return nil, "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, "", err
	}
	sid, err := user.User.Sid.Copy()
	if err != nil {
		return nil, "", err
	}
	account, domain, _, err := sid.LookupAccount("")
	if err != nil {
		return sid, sid.String(), nil
	}
	if domain != "" {
		account = domain + `\` + account
	}
	return sid, account, nil
}
