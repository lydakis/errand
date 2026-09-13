package client

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

func TestWatchSourceFailuresRemainBoundedDuringChurn(t *testing.T) {
	for _, cause := range []error{os.ErrPermission, syscall.ENOSPC, syscall.EDQUOT, syscall.EROFS, syscall.EIO, syscall.EMFILE, syscall.ENFILE} {
		err := &pushSourceError{errors.Join(fmt.Errorf("freezing source: %w", &os.PathError{Op: "open", Path: "file", Err: cause}), errors.New("source cleanup failed"))}
		if !permanentWatchSourceError(err) {
			t.Fatalf("unbounded storage/access failure: %v", err)
		}
	}
	// Disappearance during an editor's atomic save can become stable on the next
	// pass; unlike a storage failure, continued saves must not exhaust its budget.
	if permanentWatchSourceError(&pushSourceError{&os.PathError{Op: "lstat", Path: "file", Err: os.ErrNotExist}}) {
		t.Fatal("atomic-save disappearance treated as permanent")
	}
}
