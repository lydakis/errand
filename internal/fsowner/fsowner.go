// Package fsowner checks who owns an open file.
package fsowner

import "os"

// OwnedByCurrentUser reports whether f is owned by the user this process runs
// as: the effective uid on Unix, the process token's user SID on Windows.
func OwnedByCurrentUser(f *os.File) (bool, error) { return ownedByCurrentUser(f) }
