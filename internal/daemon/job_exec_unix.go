//go:build unix

package daemon

import (
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// baseEnvNames are taken from the daemon's environment into every job.
var baseEnvNames = []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "TMPDIR"}

func envNameEqual(a, b string) bool { return a == b }

func hasPathSeparator(name string) bool { return strings.ContainsRune(name, '/') }

func executableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && unix.Faccessat(unix.AT_FDCWD, path, unix.X_OK, unix.AT_EACCESS) == nil
}

// findExecutable reports whether path can be executed as named.
func findExecutable(path string) (string, bool) {
	return path, executableFile(path)
}

// checkCommandLine accepts every argument: exec passes argv unchanged.
func checkCommandLine(string, []string) error { return nil }
