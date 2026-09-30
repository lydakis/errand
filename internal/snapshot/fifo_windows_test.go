//go:build windows

package snapshot

import (
	"errors"
	"testing"
)

const fifosSupported = false

// requireFIFOs skips tests that need named pipes in the file system.
func requireFIFOs(t *testing.T) { t.Skip("Windows has no FIFOs in the file system") }

func mkfifo(string) error { return errors.New("Windows has no FIFOs in the file system") }
