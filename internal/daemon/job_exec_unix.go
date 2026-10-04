//go:build unix

package daemon

import (
	"strings"
)

// baseEnvNames are taken from the daemon's environment into every job.
// XDG_RUNTIME_DIR and DBUS_SESSION_BUS_ADDRESS come from the systemd user
// manager that runs the Linux runner, so jobs can use systemctl --user and
// other user-session services the way a login shell can.
var baseEnvNames = []string{
	"PATH", "HOME", "USER", "LOGNAME", "LANG", "TMPDIR",
	"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS",
}

func envNameEqual(a, b string) bool { return a == b }

func hasPathSeparator(name string) bool { return strings.ContainsRune(name, '/') }

func jobPATHEXT([]string) string { return "" }

// checkCommandLine accepts every argument: exec passes argv unchanged.
func checkCommandLine(string, []string) error { return nil }

// Unix executable lookup does not depend on PATHEXT.
func executableFinder(string) func(string) (string, bool) { return findExecutable }
