package proto

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
)

func TestLeaseTargetCheck(t *testing.T) {
	const hostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUF errand-lease"
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

const (
	testMacKey  = "AAAAC3NzaC1lZDI1NTE5AAAAIAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB"
	testMiniKey = "AAAAC3NzaC1lZDI1NTE5AAAAIAICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgIC"
)

// A public half rebuilt from its private key has no comment, and is still
// the same key.
func TestSameSSHKey(t *testing.T) {
	const key = "ssh-ed25519 " + testMacKey + " errand"
	for _, same := range []string{"ssh-ed25519 " + testMacKey, "ssh-ed25519  " + testMacKey + " other comment", key} {
		if !SameSSHKey(key, same) {
			t.Errorf("%q differs from %q", same, key)
		}
	}
	for _, other := range []string{"ssh-ed25519 " + testMiniKey + " errand", "ssh-rsa " + testMacKey + " errand", "", testMacKey, key + "\n" + key} {
		if SameSSHKey(key, other) {
			t.Errorf("%q matches %q", other, key)
		}
	}
}

// A key is accepted only when its data is a well-formed public key of the
// type it names, so a cloud peer cannot pass on a host key that ssh would
// refuse to use.
func TestSSHKeyBody(t *testing.T) {
	str := func(b []byte) []byte { return binary.BigEndian.AppendUint32(nil, uint32(len(b))) }
	wire := func(fields ...[]byte) string {
		var out []byte
		for _, f := range fields {
			out = append(append(out, str(f)...), f...)
		}
		return base64.StdEncoding.EncodeToString(out)
	}
	ed := bytes.Repeat([]byte{1}, 32)
	p256, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p384, err := ecdh.P384().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	point := p256.PublicKey().Bytes()
	offCurve := append([]byte{4}, bytes.Repeat([]byte{1}, 64)...)
	modulus := append([]byte{0}, bytes.Repeat([]byte{0xc5}, 256)...)
	for _, key := range []string{
		"ssh-ed25519 " + wire([]byte("ssh-ed25519"), ed),
		"ssh-ed25519 " + wire([]byte("ssh-ed25519"), ed) + " with a comment",
		"sk-ssh-ed25519@openssh.com " + wire([]byte("sk-ssh-ed25519@openssh.com"), ed, []byte("ssh:")),
		"ssh-rsa " + wire([]byte("ssh-rsa"), []byte{1, 0, 1}, modulus),
		"ecdsa-sha2-nistp256 " + wire([]byte("ecdsa-sha2-nistp256"), []byte("nistp256"), point),
		"ecdsa-sha2-nistp384 " + wire([]byte("ecdsa-sha2-nistp384"), []byte("nistp384"), p384.PublicKey().Bytes()),
		"sk-ecdsa-sha2-nistp256@openssh.com " + wire([]byte("sk-ecdsa-sha2-nistp256@openssh.com"), []byte("nistp256"), point, []byte("ssh:")),
	} {
		if _, ok := SSHKeyBody(key); !ok {
			t.Errorf("refused %q", key)
		}
	}
	// The same data spelled with unused bits set, which a strict decoder
	// refuses, so no key has two spellings.
	sk := wire([]byte("sk-ssh-ed25519@openssh.com"), ed, []byte("ssh:"))
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	last := strings.TrimRight(sk, "=")
	loose := last[:len(last)-1] + string(alphabet[strings.IndexByte(alphabet, last[len(last)-1])|1]) + sk[len(last):]
	if !strings.HasSuffix(sk, "=") || loose == sk {
		t.Fatalf("test key %q has no unused bits", sk)
	}
	for name, key := range map[string]string{
		"no data":            "ssh-ed25519 AAAA",
		"not base64":         "ssh-ed25519 not*base64",
		"loose base64":       "sk-ssh-ed25519@openssh.com " + loose,
		"other type inside":  "ssh-ed25519 " + wire([]byte("ssh-rsa"), ed),
		"short key":          "ssh-ed25519 " + wire([]byte("ssh-ed25519"), ed[:31]),
		"trailing data":      "ssh-ed25519 " + wire([]byte("ssh-ed25519"), ed, []byte("x")),
		"truncated":          "ssh-ed25519 " + base64.StdEncoding.EncodeToString(append(append(str([]byte("ssh-ed25519")), "ssh-ed25519"...), 0, 0, 0, 32, 1)),
		"no application":     "sk-ssh-ed25519@openssh.com " + wire([]byte("sk-ssh-ed25519@openssh.com"), ed),
		"negative modulus":   "ssh-rsa " + wire([]byte("ssh-rsa"), []byte{1, 0, 1}, modulus[1:]),
		"padded exponent":    "ssh-rsa " + wire([]byte("ssh-rsa"), []byte{0, 1, 0, 1}, modulus),
		"empty exponent":     "ssh-rsa " + wire([]byte("ssh-rsa"), nil, modulus),
		"other curve inside": "ecdsa-sha2-nistp256 " + wire([]byte("ecdsa-sha2-nistp256"), []byte("nistp384"), point),
		"point off curve":    "ecdsa-sha2-nistp256 " + wire([]byte("ecdsa-sha2-nistp256"), []byte("nistp256"), offCurve),
		"unknown type":       "ssh-dss " + wire([]byte("ssh-dss"), ed),
		"certificate":        "ssh-ed25519-cert-v01@openssh.com " + wire([]byte("ssh-ed25519-cert-v01@openssh.com"), ed),
		"options":            `command="sh" ssh-ed25519 ` + wire([]byte("ssh-ed25519"), ed),
	} {
		if _, ok := SSHKeyBody(key); ok {
			t.Errorf("%s: accepted %q", name, key)
		}
	}
}
