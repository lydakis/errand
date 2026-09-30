package archive

import (
	"fmt"
	"runtime"
	"strings"
)

// checkPlatformPath rejects paths this runner's file system would read
// differently from the manifest.
func checkPlatformPath(p string) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	return checkWindowsPath(p)
}

// Windows would resolve a backslash or drive in a symlink target, outside the
// escape check that works on slash-separated paths.
func platformSymlinkTarget(target string) bool {
	return runtime.GOOS != "windows" || !strings.ContainsAny(target, `\:`)
}

// Windows treats a backslash as a separator, a colon as a drive or alternate
// stream, and strips trailing dots and spaces, so such names would alias or
// escape other paths. Device names open devices in any directory. GIT~1 is
// the short name of .git.
func checkWindowsPath(p string) error {
	for _, component := range strings.Split(p, "/") {
		if problem := windowsNameProblem(component); problem != "" {
			return fmt.Errorf("archive: %q can't be stored on Windows: %s", p, problem)
		}
	}
	return nil
}

func windowsNameProblem(name string) string {
	for _, c := range name {
		if c < 0x20 || strings.ContainsRune(`\:*?"<>|`, c) {
			return fmt.Sprintf("names can't contain %q", c)
		}
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return "names can't end with a dot or space"
	}
	base, _, _ := strings.Cut(name, ".")
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return base + " is a device name"
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9' {
		return base + " is a device name"
	}
	if upper := strings.ToUpper(name); strings.HasPrefix(upper, "GIT~") && strings.Trim(upper[4:], "0123456789") == "" && len(upper) > 4 {
		return "GIT~N is the short name of .git"
	}
	return ""
}
