package proto

import (
	"encoding/base64"
	"errors"
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
// else.
func SSHKeyBody(s string) (body string, ok bool) {
	fields := strings.Fields(s)
	if len(fields) < 2 || len(s) > 16<<10 || strings.ContainsAny(s, "\r\n") || !sshKeyType(fields[0]) {
		return "", false
	}
	if _, err := base64.StdEncoding.DecodeString(fields[1]); err != nil {
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

func sshKeyType(s string) bool {
	if !strings.HasPrefix(s, "ssh-") && !strings.HasPrefix(s, "ecdsa-") && !strings.HasPrefix(s, "sk-") {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '@') {
			return false
		}
	}
	return true
}
