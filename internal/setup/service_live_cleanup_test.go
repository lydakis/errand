//go:build darwin || linux

package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

type liveCleanupSystem interface {
	serviceSystem
	Remove(string) error
	ReadFile(string) ([]byte, error)
}

// An absent socket proves only that the process stopped. Check service-manager
// operations and the registration separately, and retain every cleanup failure.
func cleanupLiveService(ctx context.Context, sys liveCleanupSystem, definition string) error {
	var failures []error
	run := func(command string, args ...string) (string, error) {
		out, err := sys.Run(ctx, command, args...)
		if err != nil {
			err = fmt.Errorf("%s %s: %w (%s)", command, strings.Join(args, " "), err, strings.TrimSpace(out))
		}
		return out, err
	}
	record := func(err error) {
		if err != nil {
			failures = append(failures, err)
		}
	}
	if sys.GOOS() == "darwin" {
		target := "gui/" + uidString(sys.UID()) + "/" + LaunchAgentLabel
		out, err := run("launchctl", "bootout", target)
		if !missingLiveService(out, err) {
			record(err)
		}
		// Clear the disabled state even if a prior assertion failed mid-test.
		_, err = run("launchctl", "enable", target)
		record(err)
		out, err = run("launchctl", "print", target)
		if err == nil {
			record(errors.New("test LaunchAgent is still registered"))
		} else if !missingLiveService(out, err) {
			record(err)
		}
	} else {
		out, err := run("systemctl", "--user", "disable", "--now", "errand.service")
		if !missingLiveService(out, err) {
			record(err)
		}
	}
	if err := sys.Remove(definition); err != nil && !errors.Is(err, os.ErrNotExist) {
		record(fmt.Errorf("remove %s: %w", definition, err))
	}
	if _, err := sys.ReadFile(definition); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = errors.New("definition still exists")
		}
		record(fmt.Errorf("verify removal of %s: %w", definition, err))
	}
	if sys.GOOS() == "linux" {
		_, err := run("systemctl", "--user", "daemon-reload")
		record(err)
		out, err := run("systemctl", "--user", "show", "errand.service", "--property=LoadState", "--value")
		// systemctl may exit nonzero for a unit that no longer exists.
		if strings.TrimSpace(out) != "not-found" {
			if err == nil {
				err = fmt.Errorf("test unit is still registered (LoadState=%q)", strings.TrimSpace(out))
			}
			record(err)
		}
	}
	return errors.Join(failures...)
}

func missingLiveService(out string, err error) bool {
	if err == nil {
		return false
	}
	detail := strings.ToLower(out + " " + err.Error())
	return strings.Contains(detail, "could not find service") || strings.Contains(detail, "service not found") || strings.Contains(detail, "no such process") ||
		(strings.Contains(detail, "unit file") && strings.Contains(detail, "does not exist"))
}

type failingCleanupSystem struct {
	*fakeSystem
	removeErr error
}

func (s failingCleanupSystem) Remove(path string) error {
	if s.removeErr != nil {
		return s.removeErr
	}
	return s.fakeSystem.Remove(path)
}

func TestLiveServiceCleanupReportsFailures(t *testing.T) {
	for _, tc := range []struct {
		name, goos, command, output string
		commandErr, removeErr       error
	}{
		{"bootout", "darwin", "launchctl bootout gui/501/dev.lydakis.errand", "", errors.New("permission denied"), nil},
		{"enable", "darwin", "launchctl enable gui/501/dev.lydakis.errand", "", errors.New("permission denied"), nil},
		{"agent remains", "darwin", "launchctl print gui/501/dev.lydakis.errand", "state = waiting", nil, nil},
		{"disable", "linux", "systemctl --user disable --now errand.service", "", errors.New("bus unavailable"), nil},
		{"remove", "linux", "", "", nil, errors.New("permission denied")},
		{"reload", "linux", "systemctl --user daemon-reload", "", errors.New("bus unavailable"), nil},
		{"unit remains", "linux", "systemctl --user show errand.service --property=LoadState --value", "loaded", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, tc.goos)
			f.cmdErr["launchctl print gui/501/dev.lydakis.errand"] = errors.New("Could not find service")
			f.cmdOutput["systemctl --user show errand.service --property=LoadState --value"] = "not-found"
			if tc.command != "" {
				delete(f.cmdErr, tc.command)
				if tc.commandErr != nil {
					f.cmdErr[tc.command] = tc.commandErr
				}
				f.cmdOutput[tc.command] = tc.output
			}
			path := "/test/service"
			f.files[path] = "service definition"
			if err := cleanupLiveService(context.Background(), failingCleanupSystem{f, tc.removeErr}, path); err == nil {
				t.Fatal("cleanup failure was hidden")
			}
		})
	}
}

func TestLiveServiceCleanupAcceptsAlreadyRemovedService(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		f := newFake(t, goos)
		f.cmdErr["launchctl bootout gui/501/dev.lydakis.errand"] = errors.New("No such process")
		f.cmdErr["launchctl print gui/501/dev.lydakis.errand"] = errors.New("Could not find service")
		f.cmdErr["systemctl --user disable --now errand.service"] = errors.New("Unit file errand.service does not exist")
		f.cmdOutput["systemctl --user show errand.service --property=LoadState --value"] = "not-found"
		if err := cleanupLiveService(context.Background(), f, "/test/missing"); err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
	}
}
