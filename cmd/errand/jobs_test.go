package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

func TestKillOnlyReportsAlreadyFinishedAfterConfirmedResult(t *testing.T) {
	for _, test := range []struct {
		name                    string
		killStatus, probeStatus int
		terminal                bool
		wantCode, wantProbes    int
	}{
		{"signal failed while running", http.StatusConflict, http.StatusOK, false, 1, 1},
		{"already finished", http.StatusConflict, http.StatusOK, true, 0, 1},
		{"unconfirmed result", http.StatusConflict, http.StatusServiceUnavailable, false, 1, 1},
		{"other rejection", http.StatusForbidden, http.StatusOK, true, 1, 0},
	} {
		for _, noWait := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/no-wait=%v", test.name, noWait), func(t *testing.T) {
				probes := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost {
						w.WriteHeader(test.killStatus)
						json.NewEncoder(w).Encode(proto.APIError{Error: "signal failed"})
						return
					}
					probes++
					w.WriteHeader(test.probeStatus)
					details := proto.JobDetails{JobStatus: proto.JobStatus{State: proto.StateRunning}}
					if test.terminal {
						details.State, details.Result = proto.StateExited, &proto.Result{ExitCode: new(int)}
					}
					json.NewEncoder(w).Encode(details)
				}))
				defer server.Close()
				args := []string{"--url", server.URL}
				if noWait {
					args = append(args, "--no-wait")
				}
				args = append(args, proto.NewULID())
				var out, errOut bytes.Buffer
				code := cmdKillTo(args, &out, &errOut)
				if code != test.wantCode || probes != test.wantProbes {
					t.Fatalf("exit=%d probes=%d output=%q", code, probes, errOut.String())
				}
				if strings.Contains(errOut.String(), "already finished") != (test.wantCode == 0) {
					t.Fatalf("incorrect verdict: %q", errOut.String())
				}
				if test.wantCode != 0 && !strings.Contains(errOut.String(), "signal failed") {
					t.Fatalf("lost original rejection: %q", errOut.String())
				}
			})
		}
	}
}

func TestSuccessfulNoWaitKillDoesNotProbeStatus(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost {
			t.Error("successful no-wait kill probed status")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	var out, errOut bytes.Buffer
	if code := cmdKillTo([]string{"--url", server.URL, "--no-wait", proto.NewULID()}, &out, &errOut); code != 0 || requests != 1 {
		t.Fatalf("exit=%d requests=%d output=%q", code, requests, errOut.String())
	}
}

func TestWaitForEndCancelsSlowStatusRequest(t *testing.T) {
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			close(canceled)
			return
		case <-time.After(500 * time.Millisecond):
			json.NewEncoder(w).Encode(proto.JobDetails{JobStatus: proto.JobStatus{State: proto.StateRunning}})
		}
	}))
	defer server.Close()
	start := time.Now()
	_, ended, err := waitForEnd(server.URL, proto.NewULID(), 50*time.Millisecond)
	if elapsed := time.Since(start); ended || !errors.Is(err, context.DeadlineExceeded) || elapsed > 300*time.Millisecond {
		t.Fatalf("kill wait: ended=%v err=%v elapsed=%s", ended, err, elapsed)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("runner status request was not canceled")
	}
}

func TestWaitForEndPreservesLastPollAndDetectsCompletion(t *testing.T) {
	for _, test := range []struct {
		name                  string
		unavailable, finished bool
	}{
		{"still running", false, false},
		{"runner unavailable", true, false},
		{"finished", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.unavailable {
					http.Error(w, "runner unavailable", http.StatusServiceUnavailable)
					return
				}
				details := proto.JobDetails{JobStatus: proto.JobStatus{State: proto.StateRunning}}
				if test.finished {
					details.State, details.Result = proto.StateExited, &proto.Result{ExitCode: new(int)}
				}
				json.NewEncoder(w).Encode(details)
			}))
			defer server.Close()
			start := time.Now()
			status, ended, err := waitForEnd(server.URL, proto.NewULID(), 50*time.Millisecond)
			if elapsed := time.Since(start); ended != test.finished || (err != nil) != test.unavailable || elapsed > 300*time.Millisecond {
				t.Fatalf("kill wait: ended=%v err=%v elapsed=%s", ended, err, elapsed)
			}
			if !test.unavailable && status.State == "" {
				t.Fatal("lost the last status response")
			}
		})
	}
}
