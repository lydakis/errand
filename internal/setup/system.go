// Package setup turns a machine into an errand runner idempotently:
// discover how to reach tailscaled, write a runner config that names the
// machine's own owner, install the platform service, make the binary
// reachable for SSH callers, and prove the daemon answers. Every side
// effect goes through System so the decisions are testable without
// touching a real systemd or launchd.
package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/tailnet"
)

// System is the seam between setup's decisions and the machine.
type System interface {
	GOOS() string
	Home() (string, error)
	Username() string
	UID() int
	Executable() (string, error)
	Abs(path string) (string, error)
	Getenv(key string) string
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, mode os.FileMode) error
	Exists(path string) bool
	IsSymlink(path string) bool
	Readlink(path string) (string, error)
	Symlink(target, link string) error
	Remove(path string) error
	Writable(dir string) bool
	Run(ctx context.Context, name string, args ...string) (string, error)
	Discover(socket, cli string) (tailnet.Provider, error)
	Probe(ctx context.Context, socket string) (proto.Info, error)
	Quiesce(ctx context.Context, socket string) (string, error)
	ReleaseQuiesce(ctx context.Context, socket, token string) error
}

type QuiesceError struct {
	Status  int
	Message string
}

func (e *QuiesceError) Error() string { return e.Message }

// RealSystem performs setup on the local machine.
type RealSystem struct{}

func (RealSystem) GOOS() string { return runtime.GOOS }

func (RealSystem) Home() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("home directory %q is not absolute", home)
	}
	return home, nil
}

func (RealSystem) Username() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

func (RealSystem) UID() int { return os.Getuid() }

func (RealSystem) Abs(path string) (string, error) { return filepath.Abs(path) }
func (RealSystem) Getenv(key string) string        { return os.Getenv(key) }

func (RealSystem) Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return serviceExecutable(exe, os.Args[0]), nil
}

func serviceExecutable(exe, invocation string) string {
	// Homebrew and user-managed symlinks stay stable across upgrades. Only use
	// the invocation path if it still identifies this running executable.
	if invoked, err := exec.LookPath(invocation); err == nil {
		if absolute, err := filepath.Abs(invoked); err == nil {
			running, runningErr := os.Stat(exe)
			candidate, candidateErr := os.Stat(absolute)
			if runningErr == nil && candidateErr == nil && os.SameFile(running, candidate) {
				return absolute
			}
		}
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe
}

func (RealSystem) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (RealSystem) WriteFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (RealSystem) Exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (RealSystem) IsSymlink(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

func (RealSystem) Readlink(path string) (string, error) { return os.Readlink(path) }
func (RealSystem) Symlink(target, link string) error    { return os.Symlink(target, link) }
func (RealSystem) Remove(path string) error             { return os.Remove(path) }

func (RealSystem) Writable(dir string) bool {
	probe, err := os.CreateTemp(dir, ".errand-writable-*")
	if err != nil {
		return false
	}
	probe.Close()
	os.Remove(probe.Name())
	return true
}

func (RealSystem) Run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, text)
		}
		return text, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return text, nil
}

func (RealSystem) Discover(socket, cli string) (tailnet.Provider, error) {
	return tailnet.Discover(socket, cli)
}

// Probe asks the daemon for /v0/info over its Unix socket. The caller is
// the daemon's own user, so peer-credential authorization admits it.
func (RealSystem) Probe(ctx context.Context, socket string) (proto.Info, error) {
	client := unixHTTPClient(socket)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://errand/v0/info", nil)
	if err != nil {
		return proto.Info{}, err
	}
	res, err := client.Do(req)
	if err != nil {
		return proto.Info{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return proto.Info{}, fmt.Errorf("daemon answered %s", res.Status)
	}
	var info proto.Info
	if err := json.NewDecoder(res.Body).Decode(&info); err != nil {
		return proto.Info{}, err
	}
	return info, nil
}

func (RealSystem) Quiesce(ctx context.Context, socket string) (string, error) {
	client := unixHTTPClient(socket)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://errand/v0/setup/quiesce", http.NoBody)
	if err != nil {
		return "", err
	}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		return "", decodeQuiesceError(res)
	}
	var lease proto.SetupQuiesce
	if err := json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&lease); err != nil {
		return "", err
	}
	if lease.Token == "" {
		return "", fmt.Errorf("daemon returned an empty setup quiesce token")
	}
	return lease.Token, nil
}

func (RealSystem) ReleaseQuiesce(ctx context.Context, socket, token string) error {
	body, err := json.Marshal(proto.SetupQuiesceRelease{Token: token})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "http://errand/v0/setup/quiesce", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := unixHTTPClient(socket).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		return decodeQuiesceError(res)
	}
	return nil
}

func unixHTTPClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socket)
		},
	}}
}

func decodeQuiesceError(res *http.Response) error {
	var apiErr proto.APIError
	if err := json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&apiErr); err != nil || apiErr.Error == "" {
		return &QuiesceError{Status: res.StatusCode, Message: "daemon answered " + res.Status}
	}
	return &QuiesceError{Status: res.StatusCode, Message: apiErr.Error}
}

func uidString(uid int) string { return strconv.Itoa(uid) }
