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

func TestSSHPinArgs(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "cache dir") // ssh splits unquoted paths at spaces
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state dir"))
	target := "ubuntu@203.0.113.7"
	if args := sshPinArgs(target); args != nil {
		t.Fatalf("unpinned target got %q", args)
	}
	for _, bad := range []string{"", "ssh-ed25519", "ssh-ed25519 not*base64", "ssh-ed25519 AAAA\n@cert-authority * ssh-ed25519 AAAA", "SSH ED AAAA"} {
		if err := PinSSHHost("ssh://"+target, bad); err == nil {
			t.Fatalf("pinned %q", bad)
		}
	}
	if err := PinSSHIdentity("ssh://"+target, "relative/key"); err == nil {
		t.Fatal("pinned a relative identity file")
	}
	if err := PinSSHHost("ssh://"+target, testHostKey); err != nil {
		t.Fatal(err)
	}
	if args := sshPinArgs(target); !slices.Contains(args, "StrictHostKeyChecking=yes") || slices.Contains(args, "-i") {
		t.Fatalf("args %q", args)
	}
	if err := PinSSHIdentity("ssh://"+target, "/keys/lambda"); err != nil {
		t.Fatal(err)
	}
	// A client re-pinning the host key keeps the cloud peer's identity.
	if err := PinSSHHost("ssh://"+target, testHostKey); err != nil {
		t.Fatal(err)
	}
	args := sshPinArgs(target)
	if args[len(args)-2] != "-i" || args[len(args)-1] != "/keys/lambda" {
		t.Fatalf("args %q", args)
	}
	if sshPinArgs("ubuntu@203.0.113.8") != nil {
		t.Fatal("pin leaked to another target")
	}
	if runtime.GOOS == "windows" {
		return
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh")
	}
	// ssh -G prints the options it would use, quoting and all.
	out, err := exec.Command("ssh", append(sshPinArgs(target), "-G", target)...).Output()
	if err != nil {
		t.Fatal(err)
	}
	config := string(out)
	for _, want := range []string{"hostkeyalias " + sshPinAlias(target), "stricthostkeychecking true", "globalknownhostsfile /dev/null"} {
		if !strings.Contains(config, want+"\n") {
			t.Errorf("ssh -G lacks %q", want)
		}
	}
	dir, _ := sshPinDir()
	if !strings.Contains(config, "userknownhostsfile "+sshPinBase(dir, target)+".known_hosts\n") {
		t.Errorf("ssh -G known hosts: %s", config)
	}
	// A control master opened without the pin, or with another host key, is
	// never shared with a pinned connection.
	controlDir, _ := sshControlDirPath()
	pinned := regexp.MustCompile(`(?m)^controlpath (.*)$`).FindStringSubmatch(config)
	if pinned == nil || !strings.HasPrefix(pinned[1], filepath.Join(controlDir, "pin-")) {
		t.Fatalf("ssh -G control path: %q", pinned)
	}
	if err := PinSSHHost("ssh://"+target, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOFmZUN5Kd0kLHRZhkmAlu0HQK9h5BFXKCzIdeUCC5n4 other"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(sshPinArgs(target), `ControlPath="`+pinned[1]+`"`) {
		t.Fatal("a new host key reuses the old control path")
	}
}
