package config

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// LoadClient merges lease records from client state. Tests must not see
	// the developer's real leases.
	stateHome, err := os.MkdirTemp("", "errand-config-test-state-")
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
