package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/setup"
)

// Entrypoint tests must not inspect a developer's installed user service.
func isolateDoctorHost(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	bin := t.TempDir()
	for name, body := range map[string]string{
		"systemctl": "#!/bin/sh\nprintf 'inactive\\n'\nexit 3\n",
		"launchctl": "#!/bin/sh\nprintf 'Could not find service\\n'\nexit 113\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestDoctorLocalChecksSurviveBrokenClientConfiguration(t *testing.T) {
	writeClientConfig(t, "[broken")
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".errand.toml", []byte("[broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := cmdDoctorWith([]string{"--config", "/custom/runner.toml", "--json"}, &out, &errOut, doctorServices{
		probe: func(context.Context, string) (proto.Info, error) {
			t.Fatal("local doctor contacted a peer")
			return proto.Info{}, nil
		},
		local: func(ctx context.Context, path string) setup.Diagnosis {
			if path != "/custom/runner.toml" {
				t.Fatal(path)
			}
			return setup.Diagnosis{Checks: []setup.DiagnosticCheck{{Name: "runner", Status: "ok", Detail: "ready"}}, Info: &proto.Info{Version: "test"}, SocketPath: "/custom/socket"}
		},
	})
	var report doctorReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if code != 1 || report.OK || report.LocalInfo == nil || report.Info != nil || report.Effective != nil || report.SocketPath != "/custom/socket" {
		t.Fatalf("code %d: %s %s", code, &out, &errOut)
	}
}

func TestDoctorRejectsRemovedLocalFlagAndEmptyConfigPath(t *testing.T) {
	for _, args := range [][]string{{"--local"}, {"--config", ""}} {
		var out, errOut bytes.Buffer
		code := cmdDoctorWith(args, &out, &errOut, doctorServices{})
		if code != 2 || out.Len() != 0 {
			t.Fatalf("%v: %d %s", args, code, &out)
		}
	}
}

func TestDoctorWithoutPeerStillChecksConfigurationAndLocalInstallation(t *testing.T) {
	for _, project := range []string{"", "[run]\npeer='missing'\n", "[run]\npeer=''\n", "[env]\npass=['ERRAND_CONSENT_TEST']\n"} {
		writeClientConfig(t, "")
		t.Chdir(t.TempDir())
		if err := os.WriteFile(".errand.toml", []byte(project), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		calls := 0
		code := cmdDoctorWith([]string{"--json"}, &out, &errOut, doctorServices{
			local: func(context.Context, string) setup.Diagnosis {
				calls++
				return setup.Diagnosis{Checks: []setup.DiagnosticCheck{{Name: "runner", Status: "skipped", Detail: "Local runner not configured."}}}
			},
			probe: func(context.Context, string) (proto.Info, error) {
				t.Fatal("unexpected peer probe")
				return proto.Info{}, nil
			},
		})
		var report doctorReport
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if calls != 1 || report.OK != (project == "") || (code == 0) != (project == "") {
			t.Fatalf("%q: %d %s", project, code, &out)
		}
	}
}

func TestDoctorCombinesLocalRunnerWithSelectedPeer(t *testing.T) {
	writeClientConfig(t, "default_peer='first'\n[peers.first]\nurl='http://first.invalid'\n[peers.second]\nurl='http://second.invalid'\n")
	t.Chdir(t.TempDir())
	for _, override := range []bool{false, true} {
		for _, localFailure := range []bool{false, true} {
			var out, errOut bytes.Buffer
			args := []string{"--json", "--config", "/custom/runner.toml"}
			wantURL := "http://first.invalid"
			if override {
				args = append(args, "--on", "second")
				wantURL = "http://second.invalid"
			}
			probes := 0
			code := cmdDoctorWith(args, &out, &errOut, doctorServices{
				local: func(_ context.Context, path string) setup.Diagnosis {
					if path != "/custom/runner.toml" {
						t.Fatal(path)
					}
					if localFailure {
						return setup.Diagnosis{Checks: []setup.DiagnosticCheck{{Name: "socket", Status: "error", Detail: "missing socket"}}}
					}
					return setup.Diagnosis{Checks: []setup.DiagnosticCheck{{Name: "runner", Status: "ok"}}, Info: &proto.Info{Version: "local-version"}, SocketPath: "/custom/socket"}
				},
				probe: func(_ context.Context, target string) (proto.Info, error) {
					probes++
					if target != wantURL {
						t.Fatal(target)
					}
					return proto.Info{Version: "peer-version"}, nil
				},
			})
			var report doctorReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if probes != 1 || report.OK == localFailure || (code == 0) == localFailure || report.Info.Version != "peer-version" {
				t.Fatalf("%d %s", code, &out)
			}
			if !localFailure && report.LocalInfo.Version != "local-version" {
				t.Fatal("local and remote info were conflated")
			}
		}
	}
}

func TestDoctorSSHFailureSkipsInfoAndKeepsConfiguredURL(t *testing.T) {
	writeClientConfig(t, "default_peer='runner'\n[peers.runner]\nssh='test@host'\nremote_command='/custom/errand'\nremote_socket='/custom/socket'\n")
	t.Chdir(t.TempDir())
	for _, fail := range []bool{false, true} {
		var out, errOut bytes.Buffer
		var selected string
		probes := 0
		code := cmdDoctorWith([]string{"--json"}, &out, &errOut, doctorServices{
			ssh: func(ctx context.Context, target string) error {
				selected = target
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("unbounded SSH check")
				}
				if fail {
					return &client.SSHDiagnosticError{CommandUnavailable: true, Detail: "missing command"}
				}
				return nil
			},
			probe: func(_ context.Context, target string) (proto.Info, error) {
				probes++
				if target != selected {
					t.Fatal("SSH check and probe used different transports")
				}
				return proto.Info{Version: "test", Proto: proto.ProtoVersion}, nil
			},
		})
		var report doctorReport
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Effective.URL != "ssh://test@host" || selected == "" {
			t.Fatalf("wrong target: %+v", report)
		}
		if fail {
			if code != 1 || probes != 0 || !strings.Contains(out.String(), "remote_command") {
				t.Fatalf("%d %s", code, &out)
			}
		} else if code != 0 || probes != 1 {
			t.Fatalf("%d %s", code, &out)
		}
	}
}

func TestDoctorRequiredEnvironmentStopsSSHCheck(t *testing.T) {
	writeClientConfig(t, "default_peer='runner'\n[peers.runner]\nssh='test@host'\n[env]\npass=['ERRAND_DOCTOR_UNSET_9754']\n")
	t.Chdir(t.TempDir())
	t.Setenv("ERRAND_DOCTOR_UNSET_9754", "")
	_ = os.Unsetenv("ERRAND_DOCTOR_UNSET_9754")
	var out, errOut bytes.Buffer
	code := cmdDoctorWith([]string{"--json"}, &out, &errOut, doctorServices{
		ssh: func(context.Context, string) error { t.Fatal("SSH check before environment consent"); return nil },
		probe: func(context.Context, string) (proto.Info, error) {
			t.Fatal("info probe before environment consent")
			return proto.Info{}, nil
		},
	})
	if code != 1 {
		t.Fatalf("%d %s", code, &out)
	}
}
