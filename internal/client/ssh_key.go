package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnsureSSHKey returns the public half of the ed25519 key at path, making
// the key first if there is none. errand makes keys of its own, without a
// passphrase, so ssh in batch mode can always use them, and makes them
// itself, so a client that never reaches a machine over SSH needs no
// OpenSSH. path's directory holds only this key and its public half, and
// is made with both: it is renamed into place whole, so a reader finds the
// pair or nothing, and of processes making the key at once, all but the
// first use the first one's.
func EnsureSSHKey(ctx context.Context, path, comment string) (string, error) {
	if public, err := readSSHKeyPair(path); !errors.Is(err, os.ErrNotExist) {
		return public, err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".key-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	private, public, err := NewSSHKey(comment)
	if err != nil {
		return "", fmt.Errorf("making an SSH key: %w", err)
	}
	made := filepath.Join(tmp, filepath.Base(path))
	if err := os.WriteFile(made, []byte(private), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(made+".pub", []byte(public+"\n"), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		// Another process published its pair first.
		if public, rerr := readSSHKeyPair(path); rerr == nil {
			return public, nil
		}
		return "", fmt.Errorf("placing SSH key %s: %w", path, err)
	}
	return public, nil
}

// readSSHKeyPair returns the public half of the key at path when both
// halves are there.
func readSSHKeyPair(path string) (string, error) {
	public, err := os.ReadFile(path + ".pub")
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	return strings.TrimSpace(string(public)), nil
}

const opensshKeyMagic = "openssh-key-v1\x00"

// NewSSHKey makes an ed25519 key: the private key in OpenSSH's format,
// without a passphrase, and the public key as an authorized_keys line
// ending in comment.
func NewSSHKey(comment string) (private, public string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	blob := sshString(nil, "ssh-ed25519")
	blob = sshString(blob, string(pub))
	var check [4]byte
	if _, err := rand.Read(check[:]); err != nil {
		return "", "", err
	}
	secret := append(check[:], check[:]...)
	secret = sshString(secret, "ssh-ed25519")
	secret = sshString(secret, string(pub))
	secret = sshString(secret, string(priv))
	secret = sshString(secret, comment)
	for i := byte(1); len(secret)%8 != 0; i++ {
		secret = append(secret, i)
	}
	data := []byte(opensshKeyMagic)
	data = sshString(data, "none") // cipher
	data = sshString(data, "none") // KDF
	data = sshString(data, "")     // KDF options
	data = binary.BigEndian.AppendUint32(data, 1)
	data = sshString(data, string(blob))
	data = sshString(data, string(secret))
	private = string(pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: data}))
	public = "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob)
	if comment != "" {
		public += " " + comment
	}
	return private, public, nil
}

// sshString appends s in the SSH wire format: its length, then its bytes.
func sshString(b []byte, s string) []byte {
	return append(binary.BigEndian.AppendUint32(b, uint32(len(s))), s...)
}
