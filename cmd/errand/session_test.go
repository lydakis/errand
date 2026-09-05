package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/daemon"
	"github.com/lydakis/errand/internal/proto"
)

func TestSessionInspectionAndDetach(t *testing.T) {
	writeClientConfig(t, "default_peer = 'test'\n[peers.test]\nurl = 'http://runner.invalid'\n[session]\nforward = ['3000']\n")
	t.Chdir(t.TempDir())
	for _, asJSON := range []bool{false, true} {
		var out, errOut bytes.Buffer
		args := []string{"--forward", "8080:3000"}
		if asJSON {
			args = append(args, "--json")
		}
		if code := cmdConfigTo(args, &out, &errOut); code != 0 || !strings.Contains(out.String(), "8080:3000") || !strings.Contains(out.String(), "cli:") {
			t.Fatalf("config: %d %s %s", code, &out, &errOut)
		}
	}
	var out, errOut bytes.Buffer
	if code := cmdConfigTo([]string{"--no-forward", "--json"}, &out, &errOut); code != 0 {
		t.Fatal(code, &errOut)
	}
	var effective config.EffectiveRun
	if err := json.Unmarshal(out.Bytes(), &effective); err != nil || effective.Forwards == nil || len(effective.Forwards) != 0 {
		t.Fatalf("clearing: %+v %v", effective, err)
	}
	out.Reset()
	errOut.Reset()
	if code := cmdDoctorTo([]string{"--json", "--no-forward"}, &out, &errOut, func(context.Context, string) (proto.Info, error) { return proto.Info{Version: "test"}, nil }); code != 0 {
		t.Fatal(code, &errOut)
	}
	t.Setenv("XDG_STATE_HOME", "invalid-state-root")
	if code := cmdRun([]string{"--detach", "--", "true"}); code != 2 {
		t.Fatalf("configured detached forward: %d", code)
	}
	for _, args := range [][]string{
		{"--forward", "3000", "--no-forward", "--", "true"},
		{"--forward", "3000", "-L", "3000:8080", "--", "true"},
	} {
		if code := cmdRun(args); code != 2 {
			t.Fatalf("conflicting mappings accepted: %d", code)
		}
	}
	if _, err := os.Stat("invalid-state-root"); !os.IsNotExist(err) {
		t.Fatal("invalid session touched state")
	}
}

func TestConfiguredForwardBindsBeforeRunAndAttach(t *testing.T) {
	// An occupied local port must prevent admission; clearing the configured
	// mapping must permit the same run. Attach uses session settings even when
	// the selected profile's run target is unrelated to its explicit handle.
	busy, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port
	d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	var requests atomic.Int64
	handler := d.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	writeClientConfig(t, fmt.Sprintf("default_peer = 'test'\n[peers.test]\nurl = %q\n[profiles.dev.run]\npeer = 'absent'\n[profiles.dev.session]\nforward = ['%d:3000']\n", server.URL, port))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	if code := cmdRun([]string{"--on", "test", "--profile", "dev", "--no-snapshot", "--", "true"}); code != 120 {
		t.Fatalf("occupied run forward: %d", code)
	}
	if code := cmdAttach([]string{"--profile", "dev", "test/" + proto.NewULID()}); code != 120 {
		t.Fatalf("occupied attach forward: %d", code)
	}
	if requests.Load() != 0 {
		t.Fatalf("occupied port contacted runner: %d requests", requests.Load())
	}
	if code := cmdRun([]string{"--on", "test", "--profile", "dev", "--no-forward", "--no-snapshot", "--", "/bin/sh", "-c", "true"}); code != 0 {
		t.Fatalf("cleared forward run: %d", code)
	}
}
