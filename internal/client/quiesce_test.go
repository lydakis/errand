package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestQuiesceRunner(t *testing.T) {
	jobs, token, refuse := 0, "", false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/setup/quiesce" {
			http.NotFound(w, r)
			return
		}
		if refuse {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(proto.APIError{Error: "runner setup is available only through the local Unix socket"})
			return
		}
		var named proto.SetupQuiesceRequest
		if r.Method == http.MethodPost {
			json.NewDecoder(r.Body).Decode(&named)
		}
		switch {
		case r.Method == http.MethodPost && named.Token != "" && named.Token == token:
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(proto.SetupQuiesce{Token: token})
		case r.Method == http.MethodPost && (jobs > 0 || token != ""):
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(proto.APIError{Error: "runner has active jobs (1 running)"})
		case r.Method == http.MethodPost:
			token = named.Token
			if token == "" {
				token = "hold"
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(proto.SetupQuiesce{Token: token})
		case r.Method == http.MethodDelete:
			var req proto.SetupQuiesceRelease
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.Token != token {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(proto.APIError{Error: "setup quiesce token does not match"})
				return
			}
			token = ""
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	ctx := context.Background()
	refuse = true
	if _, err := QuiesceRunner(ctx, server.URL, ""); !errors.Is(err, ErrQuiesceRefused) {
		t.Fatalf("refusing runner: %v", err)
	}
	refuse = false
	jobs = 1
	if _, err := QuiesceRunner(ctx, server.URL, ""); !errors.Is(err, ErrRunnerNotIdle) {
		t.Fatalf("busy runner: %v", err)
	}
	jobs = 0
	got, err := QuiesceRunner(ctx, server.URL, "")
	if err != nil || got != "hold" {
		t.Fatalf("idle runner: %q %v", got, err)
	}
	if _, err := QuiesceRunner(ctx, server.URL, ""); !errors.Is(err, ErrRunnerNotIdle) {
		t.Fatalf("held runner: %v", err)
	}
	if again, err := QuiesceRunner(ctx, server.URL, got); err != nil || again != got {
		t.Fatalf("renewing the hold: %q %v", again, err)
	}
	if _, err := QuiesceRunner(ctx, server.URL, "other"); !errors.Is(err, ErrRunnerNotIdle) {
		t.Fatalf("renewing another's hold: %v", err)
	}
	if err := ResumeRunner(ctx, server.URL, "other"); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("resumed with another token: %v", err)
	}
	if err := ResumeRunner(ctx, server.URL, got); err != nil || token != "" {
		t.Fatalf("resume: %v", err)
	}
	// A hold named by the caller is taken under that name.
	named := proto.NewULID()
	if got, err := QuiesceRunner(ctx, server.URL, named); err != nil || got != named || token != named {
		t.Fatalf("named hold: %q %v", got, err)
	}
}
