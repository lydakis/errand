package client

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSSHDiagnosticUsesConfiguredCommandWithoutExecutingIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	controlDir := filepath.Join(cache, "errand", "ssh")
	path := filepath.Join(t.TempDir(), "runner's binary")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	peer := ConfigureSSHPeer("ssh://test@host", "diagnostic-test", path, "/custom/socket")
	err = inspectSSH(context.Background(), peer, func(ctx context.Context, args []string) (string, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("SSH check is not bounded")
		}
		// Reuse Errand's authenticated master when present, without starting one
		// or updating host keys when a fresh batch connection is needed.
		want := []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "ControlMaster=no", "-o", "ControlPath=" + filepath.Join(controlDir, "%C"), "--", "test@host"}
		if !reflect.DeepEqual(args[:len(args)-1], want) {
			t.Fatalf("args %q", args)
		}
		// Resolve the literal path in a local shell; its exit-99 body must not run.
		return "", exec.CommandContext(ctx, "/bin/sh", "-c", args[len(args)-1]).Run()
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(controlDir); !os.IsNotExist(err) {
		t.Fatalf("diagnostic created a control directory: %v", err)
	}
	// The ordinary transport must compute the same directory.
	if dir, err := sshControlDir(); err != nil || dir != controlDir {
		t.Fatalf("normal transport control directory = %q, %v", dir, err)
	}
}

func TestSSHDiagnosticSeparatesCommandAndConnectionFailures(t *testing.T) {
	for _, code := range []string{"127", "255"} {
		err := inspectSSH(context.Background(), "ssh://host", func(context.Context, []string) (string, error) {
			return "fixture diagnostic", exec.Command("/bin/sh", "-c", "exit "+code).Run()
		})
		var diagnostic *SSHDiagnosticError
		if !errors.As(err, &diagnostic) || diagnostic.CommandUnavailable != (code == "127") || !strings.Contains(err.Error(), "fixture diagnostic") {
			t.Fatalf("%s: %v", code, err)
		}
	}
}
