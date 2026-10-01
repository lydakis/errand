//go:build windows

package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/filelock"
)

// Setup restarts the runner by stopping the old process and starting the new
// one at once; the new runner waits for the old one's state lock.
func TestRunnerWaitsForTheStoppingRunnersLockOnWindows(t *testing.T) {
	stateDir := t.TempDir()
	old, err := os.OpenFile(filepath.Join(stateDir, ".daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := filelock.TryLock(old); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = filelock.Unlock(old)
		_ = old.Close()
	}()
	d, err := New(Config{StateDir: stateDir, InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
}
