package proto

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
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

// sshKeyFixtures are keys SSHKeyBody accepts, and keys it refuses, naming
// those OpenSSH would still load as a public key.
func sshKeyFixtures(t *testing.T) (accepted []string, refused map[string]string, openSSHLoads map[string]bool) {
	t.Helper()
	str := func(b []byte) []byte { return binary.BigEndian.AppendUint32(nil, uint32(len(b))) }
	wire := func(fields ...[]byte) string {
		var out []byte
		for _, f := range fields {
			out = append(append(out, str(f)...), f...)
		}
		return base64.StdEncoding.EncodeToString(out)
	}
	ed := bytes.Repeat([]byte{1}, 32)
	var points [][]byte
	for _, c := range []ecdh.Curve{ecdh.P256(), ecdh.P384(), ecdh.P521()} {
		k, err := c.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		points = append(points, k.PublicKey().Bytes())
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	// SSH writes a positive integer with a leading zero when its top bit
	// is set.
	mpint := func(n *big.Int) []byte {
		b := n.Bytes()
		if b[0]&0x80 != 0 {
			b = append([]byte{0}, b...)
		}
		return b
	}
	modulus, exponent := mpint(rsaKey.N), mpint(big.NewInt(int64(rsaKey.E)))
	rsaBits := func(bits int) []byte {
		n := new(big.Int).Lsh(big.NewInt(1), uint(bits-1))
		return mpint(n.Add(n, big.NewInt(1)))
	}
	ecdsa256 := wire([]byte("ecdsa-sha2-nistp256"), []byte("nistp256"), points[0])
	accepted = []string{
		"ssh-ed25519 " + wire([]byte("ssh-ed25519"), ed),
		"ssh-ed25519 " + wire([]byte("ssh-ed25519"), ed) + " with a comment",
		"ssh-rsa " + wire([]byte("ssh-rsa"), exponent, modulus),
		"ecdsa-sha2-nistp256 " + ecdsa256,
		"ecdsa-sha2-nistp384 " + wire([]byte("ecdsa-sha2-nistp384"), []byte("nistp384"), points[1]),
		"ecdsa-sha2-nistp521 " + wire([]byte("ecdsa-sha2-nistp521"), []byte("nistp521"), points[2]),
	}
	// The same data spelled with unused bits set, which a strict decoder
	// refuses, so no key has two spellings.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	last := strings.TrimRight(ecdsa256, "=")
	loose := last[:len(last)-1] + string(alphabet[strings.IndexByte(alphabet, last[len(last)-1])|1]) + ecdsa256[len(last):]
	if !strings.HasSuffix(ecdsa256, "=") || loose == ecdsa256 {
		t.Fatalf("test key %q has no unused bits", ecdsa256)
	}
	offCurve := append([]byte{4}, bytes.Repeat([]byte{1}, 64)...)
	refused = map[string]string{
		"no data":            "ssh-ed25519 AAAA",
		"not base64":         "ssh-ed25519 not*base64",
		"loose base64":       "ecdsa-sha2-nistp256 " + loose,
		"other type inside":  "ssh-ed25519 " + wire([]byte("ssh-rsa"), ed),
		"short key":          "ssh-ed25519 " + wire([]byte("ssh-ed25519"), ed[:31]),
		"trailing data":      "ssh-ed25519 " + wire([]byte("ssh-ed25519"), ed, []byte("x")),
		"truncated":          "ssh-ed25519 " + base64.StdEncoding.EncodeToString(append(append(str([]byte("ssh-ed25519")), "ssh-ed25519"...), 0, 0, 0, 32, 1)),
		"security key":       "sk-ssh-ed25519@openssh.com " + wire([]byte("sk-ssh-ed25519@openssh.com"), ed, []byte("ssh:")),
		"tiny RSA":           "ssh-rsa AAAAB3NzaC1yc2EAAAABAQAAAAEB",
		"short modulus":      "ssh-rsa " + wire([]byte("ssh-rsa"), exponent, rsaBits(1023)),
		"long modulus":       "ssh-rsa " + wire([]byte("ssh-rsa"), exponent, rsaBits(16385)),
		"exponent 1":         "ssh-rsa " + wire([]byte("ssh-rsa"), []byte{1}, modulus),
		"even exponent":      "ssh-rsa " + wire([]byte("ssh-rsa"), []byte{1, 0, 0}, modulus),
		"negative modulus":   "ssh-rsa " + wire([]byte("ssh-rsa"), exponent, modulus[1:]),
		"padded exponent":    "ssh-rsa " + wire([]byte("ssh-rsa"), append([]byte{0}, exponent...), modulus),
		"other curve inside": "ecdsa-sha2-nistp256 " + wire([]byte("ecdsa-sha2-nistp256"), []byte("nistp384"), points[0]),
		"point off curve":    "ecdsa-sha2-nistp256 " + wire([]byte("ecdsa-sha2-nistp256"), []byte("nistp256"), offCurve),
		"unknown type":       "ssh-dss " + wire([]byte("ssh-dss"), ed),
		"certificate":        "ssh-ed25519-cert-v01@openssh.com " + wire([]byte("ssh-ed25519-cert-v01@openssh.com"), ed),
		"options":            `command="sh" ssh-ed25519 ` + wire([]byte("ssh-ed25519"), ed),
	}
	// A security key is never a host key, an authorized_keys option is not
	// part of a key, and errand wants RSA exponents that are odd, at least
	// 3 and minimally encoded, where OpenSSH takes any.
	openSSHLoads = map[string]bool{"security key": true, "options": true, "exponent 1": true, "even exponent": true, "padded exponent": true}
	return accepted, refused, openSSHLoads
}

// A key is accepted only when its data is a well-formed public key of the
// type it names, so a cloud peer cannot pass on a host key that ssh would
// refuse to use.
func TestSSHKeyBody(t *testing.T) {
	accepted, refused, _ := sshKeyFixtures(t)
	for _, key := range accepted {
		if _, ok := SSHKeyBody(key); !ok {
			t.Errorf("refused %q", key)
		}
	}
	for name, key := range refused {
		if _, ok := SSHKeyBody(key); ok {
			t.Errorf("%s: accepted %q", name, key)
		}
	}
}

// The rule follows OpenSSH: every key SSHKeyBody accepts loads in
// ssh-keygen, and the keys it refuses do not, but for the few it refuses
// on purpose.
func TestSSHKeyBodyMatchesOpenSSH(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	accepted, refused, openSSHLoads := sshKeyFixtures(t)
	loads := func(key string) bool {
		path := filepath.Join(t.TempDir(), "key.pub")
		if err := os.WriteFile(path, []byte(key+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return exec.Command("ssh-keygen", "-l", "-f", path).Run() == nil
	}
	for _, key := range accepted {
		if !loads(key) {
			t.Errorf("ssh-keygen refuses accepted %q", key)
		}
	}
	for name, key := range refused {
		if loads(key) != openSSHLoads[name] {
			t.Errorf("%s: ssh-keygen loads %q: %v", name, key, !openSSHLoads[name])
		}
	}
}
