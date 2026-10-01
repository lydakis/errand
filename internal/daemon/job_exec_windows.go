//go:build windows

package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// baseEnvNames are taken from the daemon's environment into every job. Beyond
// the Unix set, Windows programs need their profile, temp and system folders,
// and the shell needs PATHEXT and ComSpec.
var baseEnvNames = []string{
	"PATH", "PATHEXT", "LANG",
	"USERPROFILE", "HOMEDRIVE", "HOMEPATH", "USERNAME", "USERDOMAIN",
	"APPDATA", "LOCALAPPDATA", "TEMP", "TMP",
	"SystemRoot", "SystemDrive", "windir", "ComSpec", "OS", "COMPUTERNAME",
	"ProgramData", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432",
	"CommonProgramFiles", "CommonProgramFiles(x86)", "CommonProgramW6432",
	"NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
}

// Windows environment names are case-insensitive.
func envNameEqual(a, b string) bool { return strings.EqualFold(a, b) }

func hasPathSeparator(name string) bool { return strings.ContainsAny(name, `/\`) }

func executableExtensions(pathext string) []string {
	if pathext == "" {
		pathext = ".COM;.EXE;.BAT;.CMD"
	}
	var exts []string
	for _, ext := range filepath.SplitList(pathext) {
		if strings.HasPrefix(ext, ".") && len(ext) > 1 {
			exts = append(exts, strings.ToLower(ext))
		}
	}
	return exts
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// findExecutable finds what Windows would run for path: path itself when its
// extension is in PATHEXT, otherwise the first path+extension that exists.
// So "cargo" finds cargo.exe and "npm" finds npm.cmd.
func findExecutable(path string) (string, bool) {
	return executableFinder(os.Getenv("PATHEXT"))(path)
}

// Parse PATHEXT once for the entire PATH search, using the job's environment.
func executableFinder(pathext string) func(string) (string, bool) {
	exts := executableExtensions(pathext)
	return func(path string) (string, bool) {
		ext := strings.ToLower(filepath.Ext(path))
		for _, candidate := range exts {
			if ext == candidate {
				return path, regularFile(path)
			}
		}
		for _, candidate := range exts {
			if regularFile(path + candidate) {
				return path + candidate, true
			}
		}
		return "", false
	}
}

// cmd.exe runs .bat and .cmd files and reparses their command line with its
// own rules, so these characters would not reach the script as written.
const batchSpecialCharacters = "\"%^&|<>\r\n"

// checkCommandLine refuses batch-file arguments that cmd.exe would reinterpret,
// rather than run something other than what was asked.
func checkCommandLine(executable string, args []string) error {
	switch strings.ToLower(filepath.Ext(executable)) {
	case ".bat", ".cmd":
	default:
		return nil
	}
	for _, arg := range args {
		if strings.ContainsAny(arg, batchSpecialCharacters) {
			return fmt.Errorf("argument %q to %s contains characters cmd.exe would reinterpret; run it through cmd /c yourself to control quoting", arg, filepath.Base(executable))
		}
	}
	return nil
}
