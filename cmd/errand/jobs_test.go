package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

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
