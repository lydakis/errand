package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

type rejectOfflineRequest struct{ requests atomic.Int32 }

func (r *rejectOfflineRequest) RoundTrip(*http.Request) (*http.Response, error) {
	r.requests.Add(1)
	return nil, fmt.Errorf("offline command attempted a network request")
}

func TestConfigRemainsOfflineInReleaseBuild(t *testing.T) {
	writeClientConfig(t, "default_peer = 'test'\n[peers.test]\nurl = 'http://runner.invalid:7443'\n")
	oldVersion := version
	version = "0.4.0"
	t.Cleanup(func() { version = oldVersion })
	// A former opt-in must not restore analytics or create an installation ID.
	t.Setenv("ERRAND_TELEMETRY", "1")
	t.Setenv("ERRAND_TELEMETRY_DEBUG", "")
	t.Setenv("DO_NOT_TRACK", "")
	for _, key := range []string{"CI", "CONTINUOUS_INTEGRATION", "GITHUB_ACTIONS", "GITLAB_CI", "CIRCLECI", "TRAVIS", "JENKINS_URL", "BUILDKITE", "TF_BUILD", "CODEBUILD_BUILD_ID"} {
		t.Setenv(key, "")
	}
	transport := &rejectOfflineRequest{}
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	for range 2 {
		if code := runCLI([]string{"config"}); code != 0 {
			t.Fatalf("config exit = %d", code)
		}
	}
	if got := transport.requests.Load(); got != 0 {
		t.Errorf("offline config made %d network requests", got)
	}
	entries, err := os.ReadDir(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "errand"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.toml" {
		t.Fatal("offline config created files beside personal configuration")
	}
}
