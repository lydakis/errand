// Package fsmode reads the POSIX permission bits errand records for a file.
//
// On Unix they are the file's own bits. Windows has no permission bits, only
// a read-only attribute, so a Windows tree carries modes logically: files read
// as 0644 (0444 when read-only), directories as 0755, and the owner write bit
// is the only one a Windows file can confirm. Callers that know a path's
// recorded mode keep it with Inherit instead of trusting what Windows reports.
package fsmode

import "io/fs"

// Inherit returns the recorded mode for a regular file that now reports
// current. It keeps recorded bits Windows can't store, such as exec bits, and
// takes the write bit from the file.
func Inherit(recorded, current uint32) uint32 {
	if !Logical {
		return current
	}
	switch {
	case current&0o200 == 0:
		return recorded &^ 0o222
	case recorded&0o200 == 0:
		return recorded | 0o200
	}
	return recorded
}

// Matches reports whether info is consistent with the recorded permission
// bits perm.
func Matches(info fs.FileInfo, perm uint32) bool {
	if !Logical {
		return Perm(info) == perm
	}
	if !info.Mode().IsRegular() {
		return true
	}
	return info.Mode().Perm()&0o200 == fs.FileMode(perm)&0o200
}
