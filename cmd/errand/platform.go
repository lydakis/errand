package main

import "runtime"

const windowsClientUnsupported = "errand: sending jobs from Windows isn't supported yet. This PC can run jobs sent from macOS or Linux; see docs/WINDOWS.md."

// Windows is runner-only for now. Client commands would otherwise fail later
// on Unix-only checks with errors that don't say why.
func unsupportedOnThisPlatform(command string) bool {
	return runtime.GOOS == "windows" && clientCommand(command)
}

func clientCommand(command string) bool {
	switch command {
	case "serve", "setup", "config", "access", "doctor", "df", "gc", "version", "--version", "_stdio", "-h", "--help":
		return false
	}
	return true
}
