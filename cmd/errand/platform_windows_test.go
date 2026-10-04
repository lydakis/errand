//go:build windows

package main

import "testing"

func TestClientCommandsExplainWindowsIsRunnerOnlyOnWindows(t *testing.T) {
	for _, args := range [][]string{{"--on", "mac", "--", "ver"}, {"push"}, {"fetch", "mac/01J"}, {"peers"}, {"ps"}, {"workspaces"}} {
		if code := runCLI(args); code != 2 {
			t.Fatalf("errand %v exit = %d, want 2", args, code)
		}
	}
	for _, command := range []string{"serve", "setup", "access", "doctor", "version"} {
		if unsupportedOnThisPlatform(command) {
			t.Fatalf("runner command %q is refused on Windows", command)
		}
	}
}
