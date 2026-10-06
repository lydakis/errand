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

// A refusal lets the caller ask another cloud peer only when no earlier
// attempt of the request may have reached this one: a retry is refused
// without regard to what a lost first attempt started.
func TestLeaseRefusalIsSafeOnlyBeforeALostAnswer(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusPreconditionFailed, http.StatusTooManyRequests} {
		for _, lost := range []bool{false, true} {
			attempts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				if lost && attempts == 1 {
					panic(http.ErrAbortHandler)
				}
				http.Error(w, `{"error":"no"}`, status)
			}))
			_, err := AcquireLease(context.Background(), srv.URL, proto.NewULID(), "gpu", "", time.Second)
			srv.Close()
			if err == nil || LeaseRefused(err) == lost || LeaseUncertain(err) != lost {
				t.Fatalf("%d (first answer lost: %v): refused %v, uncertain %v: %v", status, lost, LeaseRefused(err), LeaseUncertain(err), err)
			}
		}
	}
	// No answer at all leaves it uncertain too.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	defer srv.Close()
	if _, err := AcquireLease(context.Background(), srv.URL, proto.NewULID(), "gpu", "", time.Second); !LeaseUncertain(err) || LeaseRefused(err) {
		t.Fatalf("unanswered: %v", err)
	}
	// Withdrawing a request the cloud peer has no lease for answers no lease.
	none := httptest.NewServer(http.NotFoundHandler())
	defer none.Close()
	if lease, err := WithdrawLeaseRequest(context.Background(), none.URL, proto.NewULID()); err != nil || lease.ID != "" {
		t.Fatalf("withdrawing an unknown request: %+v %v", lease, err)
	}
}
