package client

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lydakis/errand/internal/durable"
)

// PinSSHHost makes errand's SSH connections to peerURL, an ssh:// peer,
// accept only hostKey,
// a public key such as "ssh-ed25519 AAAA...". Leased machines are reached
// this way: their host key is known from launch, and their address may have
// belonged to another machine before. Pins live in errand's SSH cache, so
// later commands keep them.
func PinSSHHost(peerURL, hostKey string) error {
	target, ok := sshTarget(peerURL)
	if !ok {
		return fmt.Errorf("%s is not an SSH peer", peerURL)
	}
	fields := strings.Fields(hostKey)
	if len(fields) < 2 || strings.ContainsAny(hostKey, "\r\n") || !sshKeyType(fields[0]) {
		return fmt.Errorf("SSH host key for %s is not a public key", target)
	}
	if _, err := base64.StdEncoding.DecodeString(fields[1]); err != nil {
		return fmt.Errorf("SSH host key for %s is not a public key", target)
	}
	base, err := sshPinCreate(target)
	if err != nil {
		return err
	}
	return writePinFile(base+".known_hosts", sshPinAlias(target)+" "+fields[0]+" "+fields[1]+"\n")
}

// PinSSHIdentity has errand's SSH connections to a pinned peer offer
// identityFile, as a cloud peer does with the key it installed a machine
// with. Clients keep their own SSH setup and never set one.
func PinSSHIdentity(peerURL, identityFile string) error {
	target, ok := sshTarget(peerURL)
	if !ok {
		return fmt.Errorf("%s is not an SSH peer", peerURL)
	}
	if !filepath.IsAbs(identityFile) || strings.ContainsAny(identityFile, "\r\n") {
		return fmt.Errorf("SSH identity file %q must be an absolute path", identityFile)
	}
	base, err := sshPinCreate(target)
	if err != nil {
		return err
	}
	return writePinFile(base+".identity", identityFile)
}

func sshPinCreate(target string) (string, error) {
	dir, err := sshControlDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(dir, "pins"), 0o700); err != nil {
		return "", err
	}
	return sshPinBase(dir, target), nil
}

// sshPinArgs are the ssh options a pin adds. They come before any other
// option, since ssh keeps the first value it is given.
func sshPinArgs(target string) []string {
	dir, err := sshControlDirPath()
	if err != nil {
		return nil
	}
	base := sshPinBase(dir, target)
	if _, err := os.Stat(base + ".known_hosts"); err != nil || strings.Contains(base, `"`) {
		return nil
	}
	args := []string{
		"-o", "HostKeyAlias=" + sshPinAlias(target),
		"-o", `UserKnownHostsFile="` + base + `.known_hosts"`,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UpdateHostKeys=no",
	}
	if identity, err := os.ReadFile(base + ".identity"); err == nil && len(identity) > 0 {
		args = append(args, "-i", string(identity))
	}
	return args
}

func sshPinBase(dir, target string) string {
	sum := sha256.Sum256([]byte(target))
	return filepath.Join(dir, "pins", hex.EncodeToString(sum[:16]))
}

func sshPinAlias(target string) string {
	sum := sha256.Sum256([]byte(target))
	return "errand-pin-" + hex.EncodeToString(sum[:8])
}

func sshKeyType(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '@') {
			return false
		}
	}
	return s != ""
}

func writePinFile(path, content string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".pin-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := durable.Sync(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
