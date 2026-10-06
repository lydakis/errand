package proto

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math/big"
	"net/url"
	"strings"
)

// CheckPeerTransport checks the transport of a remote runner: an http(s)
// base URL, or an ssh_config host or user@host with optional absolute
// remote_command and remote_socket. Configured peers and lease targets
// follow the same rules.
func CheckPeerTransport(rawURL, ssh, remoteCommand, remoteSocket string) error {
	if rawURL != "" && ssh != "" {
		return errors.New("url and ssh are both set; choose one transport")
	}
	if ssh != "" {
		if strings.ContainsAny(ssh, " \t\r\n:/?#") || strings.HasPrefix(ssh, "@") ||
			strings.HasSuffix(ssh, "@") || strings.Count(ssh, "@") > 1 {
			return errors.New("ssh must be an ssh_config host or user@host")
		}
		if remoteSocket != "" && !strings.HasPrefix(remoteSocket, "/") {
			return errors.New("remote_socket must be an absolute Unix path")
		}
		if remoteCommand != "" && !strings.HasPrefix(remoteCommand, "/") {
			return errors.New("remote_command must be an absolute executable path")
		}
		return nil
	}
	if remoteCommand != "" || remoteSocket != "" {
		return errors.New("remote_command and remote_socket require ssh")
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("url must be an http:// or https:// runner base URL")
	}
	return nil
}

// Check reports what makes t unusable by a client. A cloud peer checks the
// target its provider returned with it, so a bad one fails the launch at
// once instead of at the client.
func (t LeaseTarget) Check() error {
	if t.URL == "" && t.SSH == "" {
		return errors.New("neither url nor ssh is set")
	}
	if err := CheckPeerTransport(t.URL, t.SSH, t.RemoteCommand, t.RemoteSocket); err != nil {
		return err
	}
	if t.HostKey != "" {
		if t.SSH == "" {
			return errors.New("host_key needs an ssh target")
		}
		if _, ok := SSHKeyBody(t.HostKey); !ok {
			return errors.New("host_key is not an SSH public key")
		}
	}
	return nil
}

// SSHKeyBody returns the key type and base64 data of one authorized_keys
// line without options, dropping its comment. ok is false for anything
// else. The key must be one OpenSSH would load as a host key: ed25519,
// NIST ECDSA or RSA, of the declared type and within OpenSSH's own limits
// for loading it, with an RSA exponent that is odd and at least 3. That is
// a check of form, matching OpenSSH's load rules, not a cryptographic
// audit. It covers host keys, the only keys from outside errand, and the
// ed25519 device keys errand makes itself.
func SSHKeyBody(s string) (body string, ok bool) {
	fields := strings.Fields(s)
	if len(fields) < 2 || len(s) > 16<<10 || strings.ContainsAny(s, "\r\n") {
		return "", false
	}
	// Strict decoding gives each key one spelling, so equal keys compare
	// equal as text.
	blob, err := base64.StdEncoding.Strict().DecodeString(fields[1])
	if err != nil || !sshPublicKeyBlob(fields[0], string(blob)) {
		return "", false
	}
	return fields[0] + " " + fields[1], true
}

// SameSSHKey reports whether a and b are the same public key, whatever
// their comments: a public half rebuilt from its private key has none.
func SameSSHKey(a, b string) bool {
	x, ok := SSHKeyBody(a)
	if !ok {
		return false
	}
	y, ok := SSHKeyBody(b)
	return ok && x == y
}

// sshPublicKeyBlob reports whether blob is a public key of keyType in the
// SSH wire format: the type again, then exactly that type's fields.
func sshPublicKeyBlob(keyType, blob string) bool {
	var name string
	if !sshRead(&blob, &name) || name != keyType {
		return false
	}
	ok := false
	switch keyType {
	case "ssh-ed25519":
		ok = sshEd25519(&blob)
	case "ssh-rsa":
		ok = sshRSA(&blob)
	case "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521":
		ok = sshECDSA(&blob, strings.TrimPrefix(keyType, "ecdsa-sha2-"))
	}
	return ok && blob == ""
}

func sshEd25519(data *string) bool {
	var key string
	return sshRead(data, &key) && len(key) == ed25519.PublicKeySize
}

// sshRSA reads an exponent and a modulus within the bounds OpenSSH loads:
// an odd exponent of at least 3 and a modulus of 1024 to 16384 bits.
func sshRSA(data *string) bool {
	var e, n *big.Int
	if !sshMpint(data, &e) || !sshMpint(data, &n) {
		return false
	}
	return e.Bit(0) == 1 && e.Cmp(big.NewInt(3)) >= 0 && n.BitLen() >= 1024 && n.BitLen() <= 16384
}

// sshMpint reads a positive integer in its shortest encoding.
func sshMpint(data *string, v **big.Int) bool {
	var n string
	if !sshRead(data, &n) || n == "" || n[0]&0x80 != 0 || n[0] == 0 && (len(n) == 1 || n[1]&0x80 == 0) {
		return false
	}
	*v = new(big.Int).SetBytes([]byte(n))
	return true
}

// sshECDSA reads a curve name and a point on that curve.
func sshECDSA(data *string, curve string) bool {
	var name, point string
	if !sshRead(data, &name) || name != curve || !sshRead(data, &point) {
		return false
	}
	c := map[string]ecdh.Curve{"nistp256": ecdh.P256(), "nistp384": ecdh.P384(), "nistp521": ecdh.P521()}[curve]
	if c == nil {
		return false
	}
	_, err := c.NewPublicKey([]byte(point))
	return err == nil
}

// sshRead takes one SSH wire string off the front of data.
func sshRead(data, s *string) bool {
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
