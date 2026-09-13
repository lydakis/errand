package client

import (
	"errors"
	"os"
	"path/filepath"
)

func (s *pushWatchState) observeGeneration(dir string) error {
	generation, err := readPushGeneration(dir)
	if err != nil {
		return err
	}
	if s.generation != generation {
		s.manifest, s.base, s.baseChecked = "", nil, false
		s.generation = generation
	}
	return nil
}

func readPushGeneration(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "push-generation"))
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(raw), err
}

// Every apply, including recovery, publishes under the checkout transfer lock
// before contacting the receiver. Other current clients can then invalidate
// their remembered checkpoint without network traffic. The token is advisory:
// pendingPush and receiver receipts remain the durable recovery authority.
// No fsync is needed because a host crash also discards every live watch cache.
func writePushGeneration(dir, id string) error {
	previous, err := readPushGeneration(dir)
	if err != nil || previous == id {
		return err
	}
	f, err := os.CreateTemp(dir, ".push-generation-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(id)
	if err := errors.Join(err, f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "push-generation"))
}
