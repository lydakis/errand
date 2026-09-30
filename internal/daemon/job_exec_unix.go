//go:build unix

package daemon

import (
	"strings"
)

// baseEnvNames are taken from the daemon's environment into every job.
var baseEnvNames = []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "TMPDIR"}

func envNameEqual(a, b string) bool { return a == b }

func hasPathSeparator(name string) bool { return strings.ContainsRune(name, '/') }

// checkCommandLine accepts every argument: exec passes argv unchanged.
func checkCommandLine(string, []string) error { return nil }
