package client

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSSHConnectFailureIsUnreachable(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho 'ssh: connect to host gone port 22: Operation timed out' >&2\nexit 255\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	_, err := ProbeInfo(context.Background(), ConfigureSSHPeer("ssh://gone", "gone", "", ""), 4*time.Second)
	if kind, _ := ProbeKindOf(err); kind != ProbeUnreachable || err.Error() != "unreachable: ssh could not connect to gone" {
		t.Fatalf("err = %v", err)
	}
}
