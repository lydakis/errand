//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
)

// Ctrl-C while the cloud peer is still answering the lease request must
// withdraw that request once the answer arrives, whatever state the lease
// is in by then; the cloud peer decides whether anyone else needs it.
func TestLeaseInterruptedDuringRequestIsWithdrawn(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	id := proto.NewULID()
	for _, tc := range []struct {
		state, withdrawn, want string
		shared                 bool
	}{
		{proto.LeaseLaunching, proto.LeaseReleasing, "released lease " + id, false},
		{proto.LeaseReady, proto.LeaseReady, "will be released unless a job is running on it", false},
		{proto.LeaseLaunching, proto.LeaseLaunching, "still held by another run", true},
	} {
		var requested, withdrawn atomic.Value
		requested.Store("")
		withdrawn.Store("")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/v0/leases":
				var req proto.LeaseRequest
				json.NewDecoder(r.Body).Decode(&req)
				requested.Store(req.RequestID)
				syscall.Kill(syscall.Getpid(), syscall.SIGINT)
				time.Sleep(200 * time.Millisecond) // the interrupt lands before the answer
				json.NewEncoder(w).Encode(proto.Lease{ID: id, Offer: "h100", State: tc.state, Shared: tc.shared})
			case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v0/lease-requests/"):
				withdrawn.Store(strings.TrimPrefix(r.URL.Path, "/v0/lease-requests/"))
				json.NewEncoder(w).Encode(proto.Lease{ID: id, Offer: "h100", State: tc.withdrawn, Shared: tc.shared})
			default:
				http.NotFound(w, r)
			}
		}))
		opt := leaseOption{Broker: placementChoice{RunCandidate: config.RunCandidate{Name: "cloud"}, Target: srv.URL}, Offer: proto.Offer{Name: "h100"}}
		_, _, err := leaseRunner(opt, "gpu", io.Discard)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) || withdrawn.Load() == "" || withdrawn.Load() != requested.Load() {
			t.Fatalf("%s: err %v, requested %q, withdrawn %q", tc.state, err, requested.Load(), withdrawn.Load())
		}
	}
}

// A cloud peer cannot slip anything but one public key into a host key, and
// a lease peer name resolves by asking its cloud peer, by any unambiguous
// ending of the lease ID.
func TestLeasePeerNames(t *testing.T) {
	bad := proto.LeaseTarget{SSH: "ubuntu@203.0.113.7", HostKey: "ssh-ed25519 AAAA\n@cert-authority * ssh-ed25519 AAAA"}
	if _, err := leaseTargetPeer(bad, ""); err == nil {
		t.Fatal("trusted a host key with a second line")
	}
	const hostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5 errand-lease"
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // no lease key yet
	a, b, c, d := "01JZ00000000000000000A7F3A", "01JZ00000000000000000B7F3A", "01JZ00000000000000000000CC", "01JZ00000000000000000000DD"
	ready := func(id, ssh string) proto.Lease {
		return proto.Lease{ID: id, Offer: "h100", State: proto.LeaseReady, Target: &proto.LeaseTarget{SSH: ssh, HostKey: hostKey}}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(proto.Info{Proto: proto.ProtoVersion, Version: version, Leases: []proto.Lease{
			ready(a, "ubuntu@203.0.113.7"), ready(b, "ubuntu@203.0.113.8"),
			{ID: c, Offer: "h100", State: proto.LeaseLaunching},
			// Leased from another of the owner's machines, whose key this
			// one does not have.
			{ID: d, Offer: "h100", State: proto.LeaseReady, SSHKey: "ssh-ed25519 bWluaQ== errand", Target: &proto.LeaseTarget{SSH: "ubuntu@203.0.113.9", HostKey: hostKey}},
		}})
	}))
	defer srv.Close()
	cfg := config.Client{Peers: map[string]config.Peer{"cloud": {URL: srv.URL}, "cloud-a7f3a": {URL: "http://taken:7443"}}}
	info, _ := client.ProbeInfo(context.Background(), srv.URL, time.Second)
	var names []string
	for _, lp := range leasePeersOf(cfg, "cloud", info) {
		names = append(names, lp.Name)
	}
	// The shortest ending that is not taken by another lease or a
	// configured peer; a launching lease, or one for another client's key,
	// is not a peer here.
	if want := []string{"cloud-0a7f3a", "cloud-b7f3a"}; !slices.Equal(names, want) {
		t.Fatalf("names %q, want %q", names, want)
	}
	if peer, ok, err := findLeasePeer(cfg, "cloud-b7f3a"); err != nil || !ok || peer.SSH != "ubuntu@203.0.113.8" {
		t.Fatalf("found %+v %v %v", peer, ok, err)
	}
	for name, want := range map[string]string{"cloud-7f3a": "more than one lease", "cloud-00cc": "no ready lease of yours", "cloud-00dd": "no ready lease of yours"} {
		if _, _, err := findLeasePeer(cfg, name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A cloud peer whose offers were removed still lists the leases it
	// has left.
	if brokers, code := leaseBrokers(cfg, "cloud", io.Discard); code != 0 || len(brokers) != 1 {
		t.Fatalf("cloud peer without offers: %v %d", brokers, code)
	}
	// Placement can rent from the implicit local peer, so it is asked for
	// leases too (here it is not running).
	var stderr strings.Builder
	if _, code := leaseBrokers(config.Client{DefaultPeer: "local"}, "local", &stderr); code == 0 || strings.Contains(stderr.String(), "unknown peer") {
		t.Fatalf("the implicit local peer is not asked for leases: %d %s", code, stderr.String())
	}
	for _, name := range []string{"cloud", "elsewhere-7f3a", "cloud-xyz", "cloud-il0u"} {
		if _, ok, err := findLeasePeer(cfg, name); ok || err != nil {
			t.Errorf("%s is not a lease name: %v %v", name, ok, err)
		}
	}
}
