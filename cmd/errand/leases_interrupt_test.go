//go:build !windows

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
)

// Ctrl-C while the cloud peer is still answering the lease request must
// release the lease it started, which only its answer names.
func TestLeaseInterruptedDuringRequestIsReleased(t *testing.T) {
	id := proto.NewULID()
	var released atomic.Value
	released.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v0/leases":
			syscall.Kill(syscall.Getpid(), syscall.SIGINT)
			time.Sleep(200 * time.Millisecond) // the interrupt lands before the answer
			json.NewEncoder(w).Encode(proto.Lease{ID: id, Offer: "h100", State: proto.LeaseLaunching})
		case r.Method == http.MethodDelete && r.URL.Path == "/v0/leases/"+id:
			released.Store(id)
			json.NewEncoder(w).Encode(proto.Lease{ID: id, Offer: "h100", State: proto.LeaseReleasing})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	opt := leaseOption{Broker: placementChoice{RunCandidate: config.RunCandidate{Name: "cloud"}, Target: srv.URL}, Offer: proto.Offer{Name: "h100"}}
	_, err := leaseRunner(opt, "gpu", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "released lease "+id) || released.Load() != id {
		t.Fatalf("err %v, released %q", err, released.Load())
	}
}
