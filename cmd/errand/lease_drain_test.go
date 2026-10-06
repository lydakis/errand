package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

// A runner that holds under a token other than the one asked for gives a
// hold the cloud peer cannot renew or lift by its own: the drain fails and
// that hold is given back.
func TestHoldRunnerRequiresTheNamedToken(t *testing.T) {
	sent, other := proto.NewULID(), proto.NewULID()
	for _, tc := range []struct {
		name, answer string
		ok           bool
	}{{"same token", sent, true}, {"other token", other, false}} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var lifted []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPost:
					var req proto.SetupQuiesceRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token != sent {
						t.Errorf("hold asked under %q: %v", req.Token, err)
					}
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(proto.SetupQuiesce{Token: tc.answer})
				case http.MethodDelete:
					var rel proto.SetupQuiesceRelease
					_ = json.NewDecoder(r.Body).Decode(&rel)
					mu.Lock()
					lifted = append(lifted, rel.Token)
					mu.Unlock()
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer srv.Close()
			err := holdRunner(context.Background(), srv.URL, sent)
			mu.Lock()
			defer mu.Unlock()
			if tc.ok {
				if err != nil || len(lifted) != 0 {
					t.Fatalf("hold under the named token: %v, lifted %q", err, lifted)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "instead of "+sent) {
				t.Fatalf("hold under another token accepted: %v", err)
			}
			if len(lifted) != 1 || lifted[0] != other {
				t.Fatalf("lifted %q, want the runner's own token %s", lifted, other)
			}
		})
	}
}
