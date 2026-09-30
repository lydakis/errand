// Package serviceruntime keeps a daemon's executable outside package-manager
// installations. Runtime files are immutable and retained across upgrades.
package serviceruntime

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Reexec moves the current process to a retained runtime before it opens any
// listeners. It returns only when already running that file or on failure.
func Reexec(stateDir string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	source, err := os.Open(executable)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := prepare(source, stateDir)
	if err != nil {
		return err
	}
	return execPrepared(source, target)
}

// Prepare publishes an executable by content hash without replacing any existing
// generation. Callers re-execute this path before opening daemon listeners.
// No runtime files are collected here: another daemon may still be using them.
func Prepare(executable, stateDir string) (string, error) {
	source, err := os.Open(executable)
	if err != nil {
		return "", err
	}
	defer source.Close()
	return prepare(source, stateDir)
}

// Path reports where Prepare would publish an executable, without writing.
func Path(executable, stateDir string) (string, error) {
	source, err := os.Open(executable)
	if err != nil {
		return "", err
	}
	defer source.Close()
	dir, err := Directory(stateDir)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, source); err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("%x", hash.Sum(nil)), runtimeName), nil
}

// Directory is where runtime generations for stateDir live.
func Directory(stateDir string) (string, error) {
	return filepath.Abs(filepath.Join(stateDir, "runtime"))
}

func prepare(source *os.File, stateDir string) (string, error) {
	dir, err := Directory(stateDir)
	if err != nil {
		return "", err
	}
	if err := privateDirectory(dir); err != nil {
		return "", err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, source); err != nil {
		return "", err
	}
	want := hash.Sum(nil)
	target := filepath.Join(dir, fmt.Sprintf("%x", want), runtimeName)
	if err := privateDirectory(filepath.Dir(target)); err != nil {
		return "", err
	}
	// Reuse needs no staging writes or extra binary-sized free space. This
	// includes the startup immediately following our own re-exec.
	if err := validateExecutable(target, want); err == nil {
		return filepath.EvalSymlinks(target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(dir, ".install-")
	if err != nil {
		return "", err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	hash.Reset()
	if _, err := io.Copy(io.MultiWriter(temp, hash), source); err != nil {
		return "", err
	}
	if !bytes.Equal(hash.Sum(nil), want) {
		return "", fmt.Errorf("installation changed while preparing runtime")
	}
	if err := temp.Chmod(0500); err != nil {
		return "", err
	}
	if err := temp.Sync(); err != nil {
		return "", err
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	// Link publishes atomically and fails if another startup already installed
	// this generation. Never rename over a file that a process may be executing.
	if err := os.Link(temp.Name(), target); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if err := validateExecutable(target, want); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(target)
}

func validateExecutable(target string, want []byte) error {
	info, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !runtimeModeOK(info) {
		return fmt.Errorf("invalid runtime executable: %s", target)
	}
	existing, err := os.Open(target)
	if err != nil {
		return err
	}
	defer existing.Close()
	actual := sha256.New()
	if _, err := io.Copy(actual, existing); err != nil {
		return err
	}
	if !bytes.Equal(actual.Sum(nil), want) {
		return fmt.Errorf("runtime executable content changed: %s", target)
	}
	return nil
}
