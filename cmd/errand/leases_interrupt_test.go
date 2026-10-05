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

// An ssh lease target's host key is pinned before anything connects, and a
// cloud peer cannot slip anything but one public key into the pin.
func TestLeasePeerPinsHostKey(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	target := proto.LeaseTarget{SSH: "ubuntu@203.0.113.7", HostKey: "ssh-ed25519 AAAA\n@cert-authority * ssh-ed25519 AAAA"}
	if _, _, err := leasePeer("cloud-7f3a", target); err == nil {
		t.Fatal("pinned a host key with a second line")
	}
	target.HostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5 errand-lease"
	if _, peerURL, err := leasePeer("cloud-7f3a", target); err != nil || peerURL != "ssh://ubuntu@203.0.113.7" {
		t.Fatalf("%q %v", peerURL, err)
	}
}
