package proto

import "testing"

func TestLeaseTargetCheck(t *testing.T) {
	const hostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5 errand-lease"
	for _, ok := range []LeaseTarget{
		{URL: "http://box:7443"},
		{URL: "https://box.example/"},
		{SSH: "box"},
		{SSH: "ubuntu@203.0.113.7", HostKey: hostKey, RemoteCommand: "/usr/local/bin/errand", RemoteSocket: "/run/errand.sock"},
	} {
		if err := ok.Check(); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	for _, bad := range []LeaseTarget{
		{},
		{URL: "http://box:7443", SSH: "box"},
		{URL: "unix:///run/errand.sock"},
		{URL: "file:///etc/passwd"},
		{URL: "http://user@box:7443"},
		{URL: "http://box:7443", RemoteCommand: "/bin/errand"},
		{URL: "http://box:7443", HostKey: hostKey},
		{SSH: "-oProxyCommand=sh x"},
		{SSH: "ubuntu@box:22"},
		{SSH: "a@b@c"},
		{SSH: "box", RemoteSocket: "run/errand.sock"},
		{SSH: "box", RemoteCommand: "errand"},
		{SSH: "box", HostKey: "ssh-ed25519"},
		{SSH: "box", HostKey: "ssh-ed25519 not*base64"},
		{SSH: "box", HostKey: "ssh-ed25519 AAAA\n@cert-authority * ssh-ed25519 AAAA"},
		{SSH: "box", HostKey: `command="sh" ssh-ed25519 AAAA`},
	} {
		if err := bad.Check(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// A public half rebuilt from its private key has no comment, and is still
// the same key.
func TestSameSSHKey(t *testing.T) {
	const key = "ssh-ed25519 bWFj errand"
	for _, same := range []string{"ssh-ed25519 bWFj", "ssh-ed25519  bWFj other comment", key} {
		if !SameSSHKey(key, same) {
			t.Errorf("%q differs from %q", same, key)
		}
	}
	for _, other := range []string{"ssh-ed25519 bWluaQ== errand", "ssh-rsa bWFj errand", "", "bWFj", "ssh-ed25519 bWFj\nssh-ed25519 bWFj"} {
		if SameSSHKey(key, other) {
			t.Errorf("%q matches %q", other, key)
		}
	}
}
