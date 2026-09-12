package main

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Automatic apply re-executes the current binary. A test binary must run
	// the worker too, rather than recursively starting the whole test suite.
	if len(os.Args) > 1 && os.Args[1] == "_automatic-apply" {
		os.Exit(cmdAutomaticApply(os.Args[2:]))
	}
	// Inspection reads local apply records. Tests must not inspect or modify
	// the developer's real client state.
	stateHome, err := os.MkdirTemp("", "errand-cli-test-state-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("XDG_STATE_HOME", stateHome); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(stateHome)
	os.Exit(code)
}
