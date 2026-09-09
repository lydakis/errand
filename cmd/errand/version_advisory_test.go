package main

import (
	"bytes"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/daemon"
)

func TestCLIRunsWithDifferentDaemonVersion(t *testing.T) {
	if peer := os.Getenv("ERRAND_VERSION_TEST_PEER"); peer != "" {
		os.Args = []string{"errand", "--url", peer, "--no-snapshot", "--no-apply", "--", "echo", "version advisory"}
		main()
		return
	}
	d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true, Version: "different-version"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	server := httptest.NewServer(d.Handler())
	defer server.Close()
	writeClientConfig(t, "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	command := exec.Command(os.Args[0], "-test.run=^TestCLIRunsWithDifferentDaemonVersion$")
	command.Dir = t.TempDir()
	command.Env = append(os.Environ(), "ERRAND_VERSION_TEST_PEER="+server.URL)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil || !strings.Contains(output.String(), "version advisory") {
		t.Fatalf("version difference blocked execution: %v %s", err, &output)
	}
}
