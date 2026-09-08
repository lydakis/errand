package setup

import (
	"fmt"
	"path/filepath"
)

// Local-only setup must not restart a preserved command that may read a
// different config or override the listener. Custom definitions need explicit
// replacement; trying to interpret arbitrary service-manager syntax is unsafe.
func preflightLocalService(sys System, home, exe, configPath, runnerPath string) error {
	var path, desired string
	switch sys.GOOS() {
	case "linux":
		path = filepath.Join(home, linuxUnitSubdir, DefaultServiceName+".service")
		desired = renderSystemdUnit(exe, configPath, runnerPath)
	case "darwin":
		path = filepath.Join(home, darwinAgentSubdir, LaunchAgentLabel+".plist")
		desired = renderLaunchAgent(LaunchAgentLabel, exe, configPath, filepath.Join(home, darwinLogSubdir, "errand.log"), runnerPath)
	default:
		return nil
	}
	if !sys.Exists(path) {
		return nil
	}
	current, err := sys.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading service definition %s: %w", path, err)
	}
	if string(current) != desired {
		return fmt.Errorf("cannot confirm local-only setup with the differing service definition at %s; inspect it and rerun setup --local --force to replace it", path)
	}
	return nil
}
