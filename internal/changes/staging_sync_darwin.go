package changes

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Sync a file or directory to the device without draining its hardware cache.
// A completed member sync alone does not establish durability. Callers must
// complete syncStagingBarrier on the same device before depending on that
// durability or publishing a durable receipt. This serves private staging and
// grouped installation; their protocols still determine rename/backup ordering.
// Apple's fcntl(2) guarantees this persists preceding fsyncs on the same device:
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/man/man2/fcntl.2
func syncStagedData(file *os.File) error {
	conn, err := file.SyscallConn()
	if err != nil {
		return err
	}
	var syncErr error
	err = conn.Control(func(fd uintptr) {
		for {
			syncErr = unix.Fsync(int(fd))
			if syncErr != unix.EINTR {
				return
			}
		}
	})
	return errors.Join(err, syncErr)
}
