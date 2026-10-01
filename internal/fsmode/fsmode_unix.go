//go:build !windows && !errand_logicalmodes

package fsmode

import "io/fs"

// Logical is true where the file system cannot store POSIX modes.
const Logical = false

// Perm returns the permission bits errand records for info.
func Perm(info fs.FileInfo) uint32 { return uint32(info.Mode().Perm()) }
