package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func TestDoctorUsesResolvedProfileAndKeepsRawURLs(t *testing.T) {
	writeClientConfig(t, "default_peer = 'wrong'\n[peers.target]\nurl = 'http://target.invalid'\n")
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".errand.toml", []byte("[profiles.check.run]\npeer = 'target'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args   []string
		target string
	}{
		{[]string{"--profile", "check"}, "http://target.invalid"},
		{[]string{"--url", "ssh://user@host"}, "ssh://user@host"},
	} {
		var out, errOut bytes.Buffer
		calls := 0
		probe := func(ctx context.Context, target string) (proto.Info, error) {
			calls++
			if target != tc.target {
				t.Fatalf("target=%q, want %q", target, tc.target)
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("probe has no deadline")
			}
			return proto.Info{Version: "test", Proto: proto.ProtoVersion}, nil
		}
		code := cmdDoctorTo(append(tc.args, "--json"), &out, &errOut, probe)
		var result doctorReport
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if code != 0 || !result.OK || calls != 1 || result.Effective.URL != tc.target {
			t.Fatalf("doctor: code=%d, report=%+v, stderr=%s", code, result, &errOut)
		}
	}
}

func TestDoctorConfigFailureSkipsProbe(t *testing.T) {
	writeClientConfig(t, "[broken")
	t.Chdir(t.TempDir())
	var out, errOut bytes.Buffer
	code := cmdDoctorTo([]string{"--json"}, &out, &errOut, func(context.Context, string) (proto.Info, error) {
		t.Fatal("probed with invalid config")
		return proto.Info{}, nil
	})
	var result doctorReport
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if code != 1 || result.OK || result.Effective != nil || result.Checks[0].Status != "error" || result.Checks[1].Status != "skipped" {
		t.Fatalf("doctor: code=%d, report=%+v", code, result)
	}
}

func TestDoctorProbeDiagnostics(t *testing.T) {
	writeClientConfig(t, "default_peer = 'test'\n[peers.test]\nurl = 'http://runner.invalid'\n")
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		kind         client.ProbeKind
		status, hint string
		code         int
	}{
		{"", "warning", "", 0},
		{client.ProbeForbidden, "error", "access list", 1},
		{client.ProbeUnreachable, "error", "connectivity", 1},
		{client.ProbeNotErrand, "error", "protocol", 1},
	} {
		var out, errOut bytes.Buffer
		code := cmdDoctorTo([]string{"--json"}, &out, &errOut, func(context.Context, string) (proto.Info, error) {
			if tc.kind != "" {
				return proto.Info{}, &client.ProbeError{Kind: tc.kind, Detail: "test diagnostic"}
			}
			return proto.Info{Version: "test", Busy: true}, nil
		})
		var result doctorReport
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		check := result.Checks[1]
		if code != tc.code || result.OK != (tc.code == 0) || check.Status != tc.status || !strings.Contains(check.Hint, tc.hint) {
			t.Fatalf("%s: code=%d report=%+v", tc.kind, code, result)
		}
	}
}

func TestDoctorOnlyRequestsInfoAndDoesNotResumeApplies(t *testing.T) {
	if os.Getenv("ERRAND_DOCTOR_ENTRYPOINT_TEST") == "1" {
		os.Args = []string{"errand", "doctor", "--json"}
		main()
		return
	}
	requests := make(chan string, 8)
	isolateDoctorHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method + " " + r.URL.Path
		_ = json.NewEncoder(w).Encode(proto.Info{Proto: proto.ProtoVersion, Version: "test"})
	}))
	defer server.Close()
	writeClientConfig(t, fmt.Sprintf("default_peer = 'test'\n[peers.test]\nurl = %q\n", server.URL))
	t.Chdir(t.TempDir())
	t.Setenv("XDG_STATE_HOME", "invalid-relative-state")
	command := exec.Command(os.Args[0], "-test.run=^TestDoctorOnlyRequestsInfoAndDoesNotResumeApplies$")
	command.Env = append(os.Environ(), "ERRAND_DOCTOR_ENTRYPOINT_TEST=1")
	var out, errOut bytes.Buffer
	command.Stdout, command.Stderr = &out, &errOut
	if err := command.Run(); err != nil || errOut.Len() != 0 {
		t.Fatalf("doctor: %v, stderr=%s", err, &errOut)
	}
	var result doctorReport
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.OK {
		t.Fatalf("report=%s, err=%v", &out, err)
	}
	if len(requests) != 1 {
		t.Fatalf("requests=%d", len(requests))
	}
	if got := <-requests; got != "GET /v0/info" {
		t.Fatalf("unexpected request %s", got)
	}
	if _, err := os.Stat("invalid-relative-state"); !os.IsNotExist(err) {
		t.Fatalf("doctor touched client state: %v", err)
	}
}

func TestDoctorUsageDoesNotProbe(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"--help"}, 0}, {[]string{"extra"}, 2},
		{[]string{"--on", "a", "--url", "http://b"}, 2},
		{[]string{"--profile="}, 2},
	} {
		var out, errOut bytes.Buffer
		code := cmdDoctorTo(tc.args, &out, &errOut, func(context.Context, string) (proto.Info, error) {
			t.Fatal("usage probed runner")
			return proto.Info{}, nil
		})
		if code != tc.code {
			t.Fatalf("%v: code=%d, stderr=%s", tc.args, code, &errOut)
		}
	}
}
