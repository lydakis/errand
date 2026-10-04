//go:build windows || errand_logicalmodes

package fsmode

import "io/fs"

// Logical is true where the file system cannot store POSIX modes. The
// errand_logicalmodes build tag turns it on elsewhere to test that path.
const Logical = true

// Perm returns the permission bits errand records for info.
func Perm(info fs.FileInfo) uint32 {
	switch {
	case info.IsDir():
		return 0o755
	case info.Mode()&fs.ModeSymlink != 0:
		return 0o777
	case info.Mode().Perm()&0o200 == 0:
		return 0o444
	default:
		return 0o644
	}
}
