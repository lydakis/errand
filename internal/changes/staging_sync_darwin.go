package changes

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Sync each staging member to the device without draining its hardware cache
// separately for every file. Callers must complete syncStagingBarrier on the
// containing tree or blob directory before publishing durable names or receipts.
// This is only for private staging on the same filesystem as that barrier.
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
