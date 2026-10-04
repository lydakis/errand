// Package filelock takes advisory whole-file locks that other processes see.
//
// Locks belong to the open file, not the process. Unlock before closing:
// Windows may release a closed file's locks late. Callers must not hold
// shared and exclusive locks on one open file at the same time.
package filelock

import (
	"errors"
	"os"
)

// ErrLocked reports that another open file holds a conflicting lock.
var ErrLocked = errors.New("file is locked by another process")

// TryLock takes an exclusive lock without waiting.
func TryLock(f *os.File) error { return tryLock(f, true) }

// TryRLock takes a shared lock without waiting.
func TryRLock(f *os.File) error { return tryLock(f, false) }

// Unlock releases a lock taken on f.
func Unlock(f *os.File) error { return unlock(f) }
