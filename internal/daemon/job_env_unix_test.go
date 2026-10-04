//go:build unix

package daemon

import (
	"slices"
	"testing"
)

func TestJobEnvironmentKeepsTheUserSession(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")
	t.Setenv("ERRAND_TEST_AMBIENT", "leak")
	env := (&Job{ID: "job"}).buildEnv()
	for _, want := range []string{"XDG_RUNTIME_DIR=/run/user/1000", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus"} {
		if !slices.Contains(env, want) {
			t.Fatalf("job environment missing %q: %v", want, env)
		}
	}
	if slices.Contains(env, "ERRAND_TEST_AMBIENT=leak") {
		t.Fatalf("job environment forwarded an ambient variable: %v", env)
	}
}
