package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTelemetryPersonalSettingSurvivesPeerEdits(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, _ := ClientPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"true", "false"} {
		if err := os.WriteFile(path, []byte("[telemetry]\nenabled = "+value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := AddPeer(path, "test", Peer{URL: "http://runner:7443"}, false); err != nil {
			t.Fatal(err)
		}
		c, err := LoadClient()
		if err != nil || c.Telemetry == nil || c.Telemetry.Enabled != (value == "true") {
			t.Fatalf("%+v %v", c, err)
		}
	}
}

func TestTelemetryCannotBeEnabledByWorkspaceOrProfile(t *testing.T) {
	for _, body := range []string{"[telemetry]\nenabled = true\n", "[profiles.test.telemetry]\nenabled = true\n"} {
		root := runFixture(t, "", body)
		if _, err := ResolveRun(root, RunOverrides{Peer: "test"}); err == nil || !strings.Contains(err.Error(), "telemetry") {
			t.Fatalf("accepted workspace telemetry: %v", err)
		}
	}
}
