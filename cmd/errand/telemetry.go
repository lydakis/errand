package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/telemetry"
)

func telemetryOperation(args []string) string {
	if len(args) == 0 || cliHelpRequested(args) {
		return ""
	}
	// Run is implicit in the CLI, rather than an accepted subcommand.
	if args[0] != "run" {
		if operation := telemetry.Operation(args[0]); operation != "" {
			return operation
		}
	}
	// Unknown arguments must never become event names or properties.
	if strings.HasPrefix(args[0], "-") {
		return "run"
	}
	return ""
}

func startTelemetry(args []string, output io.Writer) *telemetry.Reporter {
	return telemetry.New(telemetryOptions(args, output))
}

func telemetryOptions(args []string, output io.Writer) telemetry.Options {
	if telemetryOperation(args) == "" {
		return telemetry.Options{}
	}
	if os.Getenv("ERRAND_TELEMETRY_DEBUG") == "1" {
		return telemetry.Options{Preview: true, Version: version, Output: output}
	}
	if !telemetry.IsRelease(version) || telemetry.ProjectToken == "" || telemetryBlocked(os.Getenv) {
		return telemetry.Options{}
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return telemetry.Options{}
	}
	enabled := cfg.Telemetry == nil || cfg.Telemetry.Enabled
	explicit := cfg.Telemetry != nil
	if value, exists := os.LookupEnv("ERRAND_TELEMETRY"); exists {
		enabled = value == "1"
		explicit = true
	}
	if !enabled {
		return telemetry.Options{}
	}
	path, err := config.ClientPath()
	if err != nil {
		return telemetry.Options{}
	}
	return telemetry.Options{Enabled: true, NoticeRequired: !explicit, Output: output, Version: version, Token: telemetry.ProjectToken, IDPath: filepath.Join(filepath.Dir(path), "telemetry-id")}
}

func telemetryBlocked(getenv func(string) string) bool {
	if getenv("DO_NOT_TRACK") == "1" {
		return true
	}
	for _, key := range []string{"CI", "CONTINUOUS_INTEGRATION", "GITHUB_ACTIONS", "GITLAB_CI", "CIRCLECI", "TRAVIS", "JENKINS_URL", "BUILDKITE", "TF_BUILD", "CODEBUILD_BUILD_ID"} {
		value := strings.ToLower(getenv(key))
		if value != "" && value != "false" && value != "0" {
			return true
		}
	}
	return false
}

func telemetryTransport(rawURL string) string {
	scheme, _, _ := strings.Cut(rawURL, ":")
	switch scheme {
	case "http", "https", "ssh":
		return scheme
	case "unix":
		return "local"
	}
	return "unknown"
}
