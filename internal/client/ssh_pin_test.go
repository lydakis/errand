package client

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const testHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOFmZUN5Kd0kLHRZhkmAlu0HQK9h5BFXKCzIdeUCC5n3 errand-lease"

func pinArgs(t *testing.T, target string) []string {
	t.Helper()
	args, err := sshPinArgs(target)
	if err != nil {
		t.Fatal(err)
	}
	return args
}

func TestSSHPinArgs(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "cache dir") // ssh splits unquoted paths at spaces
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	target := "ubuntu@203.0.113.7"
	if args := pinArgs(t, target); args != nil {
		t.Fatalf("untrusted target got %q", args)
	}
	for _, bad := range []string{"", "ssh-ed25519", "ssh-ed25519 not*base64", "ssh-ed25519 AAAA\n@cert-authority * ssh-ed25519 AAAA", "SSH ED AAAA"} {
		if err := TrustSSHHost(target, bad, ""); err == nil {
			t.Fatalf("trusted %q", bad)
		}
	}
	if err := TrustSSHHost(target, testHostKey, "relative/key"); err == nil {
		t.Fatal("trusted a relative identity file")
	}
	if err := TrustSSHHost(target, testHostKey, ""); err != nil {
		t.Fatal(err)
	}
	if args := pinArgs(t, target); !slices.Contains(args, "StrictHostKeyChecking=yes") || slices.Contains(args, "-i") {
		t.Fatalf("args %q", args)
	}
	// A cloud peer and a client in one process each offer their own key.
	for _, identity := range []string{"/keys/lambda", "/keys/errand", "/keys/lambda"} {
		if err := TrustSSHHost(target, testHostKey, identity); err != nil {
			t.Fatal(err)
		}
	}
	args := pinArgs(t, target)
	if want := []string{"-i", "/keys/lambda", "-i", "/keys/errand"}; !slices.Equal(args[len(args)-4:], want) {
		t.Fatalf("args %q", args)
	}
	if pinArgs(t, "ubuntu@203.0.113.8") != nil {
		t.Fatal("trust leaked to another target")
	}
	if runtime.GOOS == "windows" {
		return
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh")
	}
	// ssh -G prints the options it would use, quoting and all.
	out, err := exec.Command("ssh", append(args, "-G", target)...).Output()
	if err != nil {
		t.Fatal(err)
	}
	config := string(out)
	for _, want := range []string{"hostkeyalias " + sshPinAlias(target), "stricthostkeychecking true", "globalknownhostsfile /dev/null"} {
		if !strings.Contains(config, want+"\n") {
			t.Errorf("ssh -G lacks %q", want)
		}
	}
	controlDir, _ := sshControlDirPath()
	known := regexp.MustCompile(`(?m)^userknownhostsfile (.*)$`).FindStringSubmatch(config)
	if known == nil || !strings.HasPrefix(known[1], filepath.Join(controlDir, "known-")) {
		t.Fatalf("ssh -G known hosts: %q", known)
	}
	// A control master opened without the key, or with another one, is
	// never shared with a pinned connection.
	pinned := regexp.MustCompile(`(?m)^controlpath (.*)$`).FindStringSubmatch(config)
	if pinned == nil || !strings.HasPrefix(pinned[1], filepath.Join(controlDir, "pin-")) {
		t.Fatalf("ssh -G control path: %q", pinned)
	}
	if err := TrustSSHHost(target, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOFmZUN5Kd0kLHRZhkmAlu0HQK9h5BFXKCzIdeUCC5n4 other", ""); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(pinArgs(t, target), `ControlPath="`+pinned[1]+`"`) {
		t.Fatal("a new host key reuses the old control path")
	}
}

// A later process applying a job's changes reaches a leased machine with the
// host key and identities the job was started with.
func TestRestoredSSHPeerKeepsHostKey(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	target := "ubuntu@203.0.113.9"
	restoreSSHPeer("ssh://peer-restored.errand", target, "", "", testHostKey, []string{"/keys/errand"})
	args := pinArgs(t, target)
	if !slices.Contains(args, "StrictHostKeyChecking=yes") || !slices.Contains(args, "/keys/errand") {
		t.Fatalf("args %q", args)
	}
}
