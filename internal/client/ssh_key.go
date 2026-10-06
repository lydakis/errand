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
// OpenSSH. The private key is the key: it is linked into place whole, and a
// missing public half is derived from it again.
func EnsureSSHKey(ctx context.Context, path, comment string) (string, error) {
	if public, err := os.ReadFile(path + ".pub"); err == nil {
		if _, err := os.Stat(path); err == nil {
			return strings.TrimSpace(string(public)), nil
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(dir, ".key-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		private, public, err := NewSSHKey(comment)
		if err != nil {
			return "", fmt.Errorf("making an SSH key: %w", err)
		}
		made := filepath.Join(tmp, "key")
		if err := os.WriteFile(made, []byte(private), 0o600); err != nil {
			return "", err
		}
		if err := os.WriteFile(made+".pub", []byte(public+"\n"), 0o644); err != nil {
			return "", err
		}
		// Linking never replaces a key another process made first.
		err = os.Link(made, path)
		if err == nil {
			if err := os.Rename(made+".pub", path+".pub"); err != nil {
				return "", err
			}
			return public, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	// The private key has no public half yet: another process is about to
	// publish it, or one was stopped before it could.
	private, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	public, err := sshPublicKeyOf(private)
	if err != nil {
		return "", fmt.Errorf("reading SSH key %s: %w", path, err)
	}
	pub := filepath.Join(tmp, "key.pub")
	if err := os.WriteFile(pub, []byte(public+"\n"), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(pub, path+".pub"); err != nil {
		return "", err
	}
	return public, nil
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

// sshPublicKeyOf is the authorized_keys line of an OpenSSH private key
// without a passphrase, with the key's comment.
func sshPublicKeyOf(private []byte) (string, error) {
	block, _ := pem.Decode(private)
	if block == nil || block.Type != "OPENSSH PRIVATE KEY" {
		return "", errors.New("not an OpenSSH private key")
	}
	data, ok := strings.CutPrefix(string(block.Bytes), opensshKeyMagic)
	var cipher, kdf, blob, secret string
	ok = ok && sshRead(&data, &cipher) && sshRead(&data, &kdf) && sshRead(&data, new(string)) &&
		len(data) >= 4 && binary.BigEndian.Uint32([]byte(data)) == 1
	if ok {
		data = data[4:]
		ok = sshRead(&data, &blob) && sshRead(&data, &secret)
	}
	if !ok {
		return "", errors.New("malformed OpenSSH private key")
	}
	if cipher != "none" || kdf != "none" {
		return "", errors.New("the key has a passphrase")
	}
	var keyType, comment string
	if len(secret) < 8 {
		return "", errors.New("malformed OpenSSH private key")
	}
	secret = secret[8:]
	if !sshRead(&secret, &keyType) || !sshRead(&secret, new(string)) || !sshRead(&secret, new(string)) || !sshRead(&secret, &comment) || keyType != "ssh-ed25519" {
		return "", errors.New("not an ed25519 key")
	}
	public := keyType + " " + base64.StdEncoding.EncodeToString([]byte(blob))
	if comment != "" {
		public += " " + comment
	}
	return public, nil
}

// sshString appends s in the SSH wire format: its length, then its bytes.
func sshString(b []byte, s string) []byte {
	return append(binary.BigEndian.AppendUint32(b, uint32(len(s))), s...)
}

// sshRead takes one SSH wire string off the front of data.
func sshRead(data *string, s *string) bool {
	if len(*data) < 4 {
		return false
	}
	n := binary.BigEndian.Uint32([]byte((*data)[:4]))
	if uint64(len(*data)-4) < uint64(n) {
		return false
	}
	*s, *data = (*data)[4:4+n], (*data)[4+n:]
	return true
}
