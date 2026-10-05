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
	for _, name := range []string{"cloud", "elsewhere-7f3a", "cloud-xyz", "cloud-il0u"} {
		if _, ok, err := findLeasePeer(cfg, name); ok || err != nil {
			t.Errorf("%s is not a lease name: %v %v", name, ok, err)
		}
	}
}
