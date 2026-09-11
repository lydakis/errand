package changes

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Sync each staging member to the device without draining its hardware cache
// separately for every file. CaptureWorkspaceBaseContext performs F_FULLFSYNC
// on the completed tree before publishing it, then on the renamed tree's parent.
// Apple's fcntl(2) guarantees this persists preceding fsyncs on the same device:
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/man/man2/fcntl.2
func syncCapturedData(file *os.File) error {
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
