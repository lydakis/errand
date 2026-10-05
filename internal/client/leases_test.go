package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

// A lease request whose answer is lost is sent again under the same request
// ID, so the cloud peer returns the lease it started instead of a second one.
func TestAcquireLeaseRepeatsLostRequest(t *testing.T) {
	id := proto.NewULID()
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req proto.LeaseRequest
		json.NewDecoder(r.Body).Decode(&req)
		requests = append(requests, req.RequestID)
		if len(requests) == 1 {
			// The lease starts, but the answer never arrives.
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		json.NewEncoder(w).Encode(proto.Lease{ID: id, State: proto.LeaseLaunching})
	}))
	defer srv.Close()
	lease, err := AcquireLease(context.Background(), srv.URL, proto.NewULID(), "gpu", "", time.Second)
	if err != nil || lease.ID != id {
		t.Fatalf("lease %+v %v", lease, err)
	}
	if len(requests) != 2 || !proto.ValidULID(requests[0]) || requests[0] != requests[1] {
		t.Fatalf("request IDs %q", requests)
	}

	// A refusal is an answer, so it is not repeated.
	requests = nil
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, "")
		http.Error(w, `{"error":"no offer matches"}`, http.StatusPreconditionFailed)
	}))
	defer refusing.Close()
	if _, err := AcquireLease(context.Background(), refusing.URL, proto.NewULID(), "gpu", "", time.Second); err == nil || len(requests) != 1 || strings.Contains(err.Error(), "errand leases lists it") {
		t.Fatalf("refusal: %v after %d requests", err, len(requests))
	}
}
