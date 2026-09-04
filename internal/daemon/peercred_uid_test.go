//go:build linux || darwin

package daemon

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestCurrentUIDMatchesEffectiveKernelUID(t *testing.T) {
	if got, want := currentUID(), uint32(unix.Geteuid()); got != want {
		t.Fatalf("current UID = %d, want effective kernel UID %d", got, want)
	}
}
