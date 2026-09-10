package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/telemetry"
)

func TestTelemetryConsentAndExcludedCommands(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}, {"version"}, {"setup"}, {"serve"}, {"access"}, {"_stdio"}, {"_automatic-apply"}, {"private-command"}, {"fetch", "--help"}} {
		if op := telemetryOperation(args); op != "" {
			t.Fatalf("%v: %s", args, op)
		}
	}
	if telemetryOperation([]string{"--", "my-command", "--help"}) != "run" {
		t.Fatal("inspected executed command")
	}
	for _, key := range []string{"DO_NOT_TRACK", "CI", "GITHUB_ACTIONS", "JENKINS_URL"} {
		if !telemetryBlocked(func(k string) string {
			if k == key {
				return "1"
			}
			return ""
		}) {
			t.Fatal(key)
		}
	}
	oldVersion, oldToken := version, telemetry.ProjectToken
	version, telemetry.ProjectToken = "0.2.1", "test-token"
	t.Cleanup(func() { version, telemetry.ProjectToken = oldVersion, oldToken })
	for _, key := range []string{"CI", "CONTINUOUS_INTEGRATION", "GITHUB_ACTIONS", "GITLAB_CI", "CIRCLECI", "TRAVIS", "JENKINS_URL", "BUILDKITE", "TF_BUILD", "CODEBUILD_BUILD_ID", "DO_NOT_TRACK", "ERRAND_TELEMETRY_DEBUG"} {
		t.Setenv(key, "")
	}
	writeClientConfig(t, "[telemetry]\nenabled = true\n")
	path, _ := config.ClientPath()
	oldEnv, hadEnv := os.LookupEnv("ERRAND_TELEMETRY")
	os.Unsetenv("ERRAND_TELEMETRY")
	t.Cleanup(func() {
		if hadEnv {
			os.Setenv("ERRAND_TELEMETRY", oldEnv)
		} else {
			os.Unsetenv("ERRAND_TELEMETRY")
		}
	})
	if !telemetryOptions([]string{"ps"}, io.Discard).Enabled {
		t.Fatal("personal opt-in ignored")
	}
	if telemetryOptions([]string{"ps"}, io.Discard).NoticeRequired {
		t.Fatal("explicit opt-in requires notice")
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	defaults := telemetryOptions([]string{"ps"}, io.Discard)
	if !defaults.Enabled || !defaults.NoticeRequired {
		t.Fatal("default must require notice before sending")
	}
	if err := os.WriteFile(path, []byte("[telemetry]\nenabled = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if telemetryOptions([]string{"ps"}, io.Discard).Enabled {
		t.Fatal("personal opt-out ignored")
	}
	t.Setenv("ERRAND_TELEMETRY", "1")
	if !telemetryOptions([]string{"ps"}, io.Discard).Enabled {
		t.Fatal("environment opt-in ignored")
	}
	if err := os.WriteFile(path, []byte("[telemetry]\nenabled = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ERRAND_TELEMETRY", "0")
	if r := startTelemetry([]string{"ps"}, io.Discard); r != nil {
		t.Fatal("environment opt-out ignored")
	}
	t.Setenv("ERRAND_TELEMETRY", "1")
	t.Setenv("DO_NOT_TRACK", "1")
	if r := startTelemetry([]string{"ps"}, io.Discard); r != nil {
		t.Fatal("DNT ignored")
	}
	t.Setenv("DO_NOT_TRACK", "")
	version = "0.2.1-dev"
	if r := startTelemetry([]string{"ps"}, io.Discard); r != nil {
		t.Fatal("development build emitted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "telemetry-id")); !os.IsNotExist(err) {
		t.Fatal("opt-out created state")
	}
	version = "0.2.1"
	// Preview overrides an enabled release without sending or creating an ID.
	t.Setenv("ERRAND_TELEMETRY_DEBUG", "1")
	var preview bytes.Buffer
	r := startTelemetry([]string{"ps"}, &preview)
	r.Finish("ps", 0)
	if !strings.Contains(preview.String(), "cli_finished") {
		t.Fatal(preview.String())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "telemetry-id")); !os.IsNotExist(err) {
		t.Fatal("preview created state")
	}
}

func TestRunTelemetryReportsAdmissionWithoutPrivateInputs(t *testing.T) {
	writeClientConfig(t, "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v0/jobs/") {
			_, _ = io.Copy(io.Discard, r.Body)
			_ = json.NewEncoder(w).Encode(proto.JobStatus{State: proto.StateRunning})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	var preview bytes.Buffer
	r := telemetry.New(telemetry.Options{Preview: true, Version: "0.2.1", Output: &preview})
	code := cmdRun([]string{"--url", server.URL, "--no-snapshot", "-d", "-e", "SECRET=private-value", "--", "private-executable", "private-argument"}, r)
	r.Finish("run", code)
	if code != 0 {
		t.Fatalf("run exit %d", code)
	}
	if !strings.Contains(preview.String(), `"event":"job_admitted"`) || !strings.Contains(preview.String(), `"start_detached":true`) {
		t.Fatal(preview.String())
	}
	for _, secret := range []string{server.URL, "SECRET", "private-value", "private-executable", "private-argument"} {
		if strings.Contains(preview.String(), secret) {
			t.Fatalf("leaked %q: %s", secret, preview.String())
		}
	}
	preview.Reset()
	r = telemetry.New(telemetry.Options{Preview: true, Version: "0.2.1", Output: &preview})
	code = cmdRun([]string{"--no-snapshot", "--"}, r)
	r.Finish("run", code)
	if strings.Contains(preview.String(), "job_admitted") {
		t.Fatal("reported unsubmitted job")
	}
}

func TestTelemetryParseErrorsReturnThroughCLILifecycle(t *testing.T) {
	writeClientConfig(t, "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("ERRAND_TELEMETRY_DEBUG", "1")
	for _, command := range []string{"fetch", "kill", "ps", "df"} {
		t.Run(command, func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			old := os.Stderr
			os.Stderr = f
			t.Cleanup(func() { os.Stderr = old; f.Close() })
			if code := runCLI([]string{command, "--invalid-test-flag"}); code != 2 {
				t.Fatalf("parse error exit: %d", code)
			}
			output, err := os.ReadFile(f.Name())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(output, []byte(`"event":"cli_finished"`)) || !bytes.Contains(output, []byte(`"outcome":"nonzero_exit"`)) {
				t.Fatalf("missing parse-error telemetry: %s", output)
			}
			if code := runCLI([]string{command, "--help"}); code != 0 {
				t.Fatalf("help exit: %d", code)
			}
			output, _ = os.ReadFile(f.Name())
			if bytes.Count(output, []byte("[telemetry]")) != 1 {
				t.Fatal("help emitted telemetry")
			}
		})
	}
}
