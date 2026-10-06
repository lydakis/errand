package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lydakis/errand/internal/limitbuf"
	"github.com/lydakis/errand/internal/nowindow"
	"github.com/lydakis/errand/internal/proto"
)

// sshTrust is what this process was told about one SSH target.
type sshTrust struct {
	hostKey    string
	identities []string
}

// poolKey tells transports to one target apart by what they trust, so a
// connection opened before a host key was pinned is never reused after.
func (t sshTrust) poolKey() string {
	if t.hostKey == "" && len(t.identities) == 0 {
		return ""
	}
	return t.hostKey + "\x00" + strings.Join(t.identities, "\x00")
}

var (
	sshTrustMu sync.Mutex
	sshTrusted = map[string]sshTrust{}
)

// TrustSSHHost registers what this process's SSH connections to target
// (user@host) trust: only hostKey, a public key such as "ssh-ed25519
// AAAA...", or with hostKey empty, the user's own known_hosts; and they
// offer identityFile when it is not empty. Leased machines are reached this
// way: their cloud peer may report the host key they booted with, and their
// address may have belonged to another machine before, so a registration
// with another host key, or none, replaces what an earlier one said about
// target. They admit the client's errand key either way. Nothing is kept
// between processes; each one learns the key from the cloud peer again.
func TrustSSHHost(target, hostKey, identityFile string) error {
	var key string
	if hostKey != "" {
		var ok bool
		if key, ok = proto.SSHKeyBody(hostKey); !ok {
			return fmt.Errorf("SSH host key for %s is not a public key", target)
		}
	}
	if identityFile != "" && (!filepath.IsAbs(identityFile) || strings.ContainsAny(identityFile, "\"\r\n")) {
		return fmt.Errorf("SSH identity file %q must be an absolute path", identityFile)
	}
	sshTrustMu.Lock()
	defer sshTrustMu.Unlock()
	trust := sshTrusted[target]
	if trust.hostKey != key {
		trust = sshTrust{hostKey: key}
	}
	// A cloud peer and its own client may share a process and each offer
	// their key for the same machine; ssh tries them in turn.
	if identityFile != "" && !slices.Contains(trust.identities, identityFile) {
		trust.identities = append(slices.Clone(trust.identities), identityFile)
	}
	if trust.poolKey() == "" {
		delete(sshTrusted, target) // an ordinary peer again
	} else {
		sshTrusted[target] = trust
	}
	return nil
}

func sshTrustFor(target string) (sshTrust, bool) {
	sshTrustMu.Lock()
	defer sshTrustMu.Unlock()
	trust, ok := sshTrusted[target]
	return trust, ok
}

// sshPinArgs are the ssh options what this process trusts about target
// adds, or none for a target it was told nothing about. They come before
// any other option, since ssh keeps the first value it is given.
func sshPinArgs(target string) ([]string, error) {
	trust, _ := sshTrustFor(target)
	return trust.args(target)
}

func (trust sshTrust) args(target string) ([]string, error) {
	var args []string
	if trust.hostKey != "" {
		controlDir, err := sshControlDir()
		if err != nil {
			return nil, err
		}
		alias := sshPinAlias(target)
		line := alias + " " + trust.hostKey + "\n"
		sum := sha256.Sum256([]byte(line))
		// ssh reads host keys only from files. The file is named by its
		// content, so any process may write it and none depends on it lasting.
		known := filepath.Join(controlDir, "known-"+hex.EncodeToString(sum[:16]))
		if strings.Contains(known, `"`) {
			return nil, fmt.Errorf("SSH control directory %s contains a double quote", controlDir)
		}
		if data, err := os.ReadFile(known); err != nil || string(data) != line {
			if err := writeKnownHosts(known, line); err != nil {
				return nil, err
			}
		}
		// A connection shared through a control master skips the host key
		// check, so it is shared only with connections expecting the same key.
		args = append(args,
			"-o", "HostKeyAlias="+alias,
			"-o", `UserKnownHostsFile="`+known+`"`,
			"-o", "GlobalKnownHostsFile=/dev/null",
			"-o", "StrictHostKeyChecking=yes",
			"-o", "UpdateHostKeys=no",
			"-o", `ControlPath="`+filepath.Join(controlDir, "pin-"+hex.EncodeToString(sum[:10]))+`"`,
		)
		// Only the lease's own keys are offered to a machine whose host key
		// is pinned: an agent or config with many keys could otherwise use
		// up the server's authentication attempts. Without a pin, the keys
		// are offered next to the user's own, as ssh would without errand.
		if len(trust.identities) > 0 {
			args = append(args, "-o", "IdentitiesOnly=yes")
		}
	}
	for _, identity := range trust.identities {
		args = append(args, "-i", identity)
	}
	return args, nil
}

func sshPinAlias(target string) string {
	sum := sha256.Sum256([]byte(target))
	return "errand-pin-" + hex.EncodeToString(sum[:8])
}

func writeKnownHosts(path, content string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".known-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// RunSSH runs command on target (user@host) with stdin, as one ssh session
// that trusts only what this process was told about target, if anything.
// It never prompts.
func RunSSH(ctx context.Context, target, command string, stdin io.Reader) error {
	pin, err := sshPinArgs(target)
	if err != nil {
		return err
	}
	args := append(pin, "-T", "-o", "BatchMode=yes",
		"-o", fmt.Sprintf("ConnectTimeout=%d", int((peerConnectTimeout+time.Second-1)/time.Second)),
		"--", target, command)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = stdin
	cmd.WaitDelay = time.Second
	nowindow.Hide(cmd)
	// The remote side decides how much it prints, so keep only enough to
	// explain a failure.
	stderr := &limitbuf.Buffer{Limit: 4096}
	cmd.Stdout, cmd.Stderr = io.Discard, stderr
	err = cmd.Run()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("ssh %s: %w: %s", target, err, msg)
		}
		return fmt.Errorf("ssh %s: %w", target, err)
	}
	return nil
}
