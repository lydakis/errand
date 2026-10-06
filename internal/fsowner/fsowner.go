// Package fsowner checks who owns an open file, and who else can read it.
package fsowner

import "os"

// OwnedByCurrentUser reports whether f is owned by the user this process runs
// as: the effective uid on Unix, the process token's user SID on Windows.
func OwnedByCurrentUser(f *os.File) (bool, error) { return ownedByCurrentUser(f) }

// Private reports whether no one but f's owner can read it. On Unix that
// means no group or other permission bits. On Windows it means no entry in
// f's DACL lets anyone read it but the owner, this process's user, SYSTEM
// and Administrators, who can read every file anyway.
func Private(f *os.File) (bool, error) { return private(f) }
