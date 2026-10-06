//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	}{
		{proto.LeaseLaunching, proto.LeaseReleasing, "released lease " + id},
		{proto.LeaseReady, proto.LeaseReady, "stays until idle"},
		{proto.LeaseLaunching, proto.LeaseLaunching, "keeps launching for another run"},
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
				json.NewEncoder(w).Encode(proto.Lease{ID: id, Offer: "h100", State: tc.state})
			case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v0/lease-requests/"):
				withdrawn.Store(strings.TrimPrefix(r.URL.Path, "/v0/lease-requests/"))
				json.NewEncoder(w).Encode(proto.Lease{ID: id, Offer: "h100", State: tc.withdrawn})
			default:
				http.NotFound(w, r)
			}
		}))
		opt := leaseOption{Broker: placementChoice{RunCandidate: config.RunCandidate{Name: "cloud"}, Target: srv.URL}, Offer: proto.Offer{Name: "h100"}}
		_, err := leaseRunner([]leaseOption{opt}, "gpu", io.Discard)
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
	other := proto.Lease{ID: d, Offer: "h100", State: proto.LeaseReady, SSHKeys: []string{"ssh-ed25519 bWluaQ== errand"}, Target: &proto.LeaseTarget{SSH: "ubuntu@203.0.113.9", HostKey: hostKey}}
	// Two leases share an ending, and this device is let into only one.
	e, f := "01JZ0000000000000000AEE11A", "01JZ0000000000000000BEE11A"
	otherF := other
	otherF.ID = f
	var admitted atomic.Value
	admitted.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v0/leases/"+d+"/ssh-keys" {
			var req proto.LeaseRequest
			json.NewDecoder(r.Body).Decode(&req)
			admitted.Store(req.SSHKey)
			json.NewEncoder(w).Encode(other)
			return
		}
		if r.URL.Path == "/v0/leases/"+d {
			admittedOther := other
			admittedOther.SSHKeys = append(slices.Clone(other.SSHKeys), admitted.Load().(string))
			json.NewEncoder(w).Encode(admittedOther)
			return
		}
		json.NewEncoder(w).Encode(proto.Info{Proto: proto.ProtoVersion, Version: version, Leases: []proto.Lease{
			ready(a, "ubuntu@203.0.113.7"), ready(b, "ubuntu@203.0.113.8"),
			{ID: c, Offer: "h100", State: proto.LeaseLaunching},
			// Leased from another of the owner's devices, whose key this
			// one does not have.
			other,
			ready(e, "ubuntu@203.0.113.10"), otherF,
			// Not a lease ID; a peer must not get it printed.
			ready("\x1b]0;owned\a000000000000000BAD", "ubuntu@203.0.113.11"),
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
	if want := []string{"cloud-0a7f3a", "cloud-b7f3a", "cloud-aee11a"}; !slices.Equal(names, want) {
		t.Fatalf("names %q, want %q", names, want)
	}
	if peer, ok, err := findLeasePeer(cfg, "cloud-b7f3a"); err != nil || !ok || peer.SSH != "ubuntu@203.0.113.8" {
		t.Fatalf("found %+v %v %v", peer, ok, err)
	}
	for name, want := range map[string]string{"cloud-7f3a": "more than one lease", "cloud-00cc": "no ready lease of yours", "cloud-ee11a": "more than one lease", "cloud-0bad": "no ready lease of yours"} {
		if _, _, err := findLeasePeer(cfg, name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Naming a lease another device asked for lets this one in.
	if peer, ok, err := findLeasePeer(cfg, "cloud-00dd"); err != nil || !ok || peer.SSH != "ubuntu@203.0.113.9" || !strings.HasPrefix(admitted.Load().(string), "ssh-ed25519 ") {
		t.Fatalf("other device's lease: %+v %v %v (sent %q)", peer, ok, err, admitted.Load())
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

// Once its lease is ready, a run leaves it to the cloud peer's idle rule
// whatever becomes of its job there.
func TestLeasedRunLeavesAReadyLeaseToTheIdleRule(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(http.ResponseWriter)
	}{
		{"refused", func(w http.ResponseWriter) { http.Error(w, "refused", http.StatusForbidden) }},
		{"answer lost", func(http.ResponseWriter) { panic(http.ErrAbortHandler) }},
		{"admitted", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(proto.JobStatus{State: proto.StateQueued})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			var requested, withdrawn atomic.Value
			requested.Store("")
			withdrawn.Store("")
			id := proto.NewULID()
			// One server is both the cloud peer and the leased machine.
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v0/leases":
					var req proto.LeaseRequest
					json.NewDecoder(r.Body).Decode(&req)
					requested.Store(req.RequestID)
					json.NewEncoder(w).Encode(proto.Lease{ID: id, Offer: "a10", State: proto.LeaseReady, Target: &proto.LeaseTarget{URL: srv.URL}})
				case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v0/lease-requests/"):
					withdrawn.Store(strings.TrimPrefix(r.URL.Path, "/v0/lease-requests/"))
					json.NewEncoder(w).Encode(proto.Lease{ID: id, Offer: "a10", State: proto.LeaseReady})
				case r.Method == http.MethodGet && r.URL.Path == "/v0/info":
					json.NewEncoder(w).Encode(proto.Info{Proto: proto.ProtoVersion, Version: version, MaxJobs: 1, Facts: proto.Facts{OS: "linux", Arch: "amd64", GPUs: []proto.GPU{{Name: "NVIDIA A10", MemoryMiB: 24 << 10}}}})
				case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v0/jobs/"):
					io.Copy(io.Discard, r.Body)
					tc.answer(w)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			var stderr strings.Builder
			opts := client.RunOptions{Where: "gpu", Root: t.TempDir(), NoSnapshot: true, Detach: true, Argv: []string{"true"}, Stdout: io.Discard, Stderr: &stderr}
			option := leaseOption{Broker: placementChoice{RunCandidate: config.RunCandidate{Name: "cloud"}, Target: srv.URL}, Offer: proto.Offer{Name: "a10"}}
			configurePlacement(&opts, nil, func() (placementChoice, error) {
				return leaseRunner([]leaseOption{option}, "gpu", &stderr)
			}, &stderr, func(placementChoice) {})
			client.Run(opts)
			if requested.Load() == "" || withdrawn.Load() != "" {
				t.Fatalf("requested %q, withdrawn %q\n%s", requested.Load(), withdrawn.Load(), stderr.String())
			}
		})
	}
}

// errand leases release takes a bare lease ID and finds the cloud peer holding it.
func TestLeaseIDFindsItsCloudPeer(t *testing.T) {
	id := proto.NewULID()
	lease := proto.Lease{ID: id, Offer: "a10", State: proto.LeaseReady}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/info":
			json.NewEncoder(w).Encode(proto.Info{Proto: proto.ProtoVersion, Version: version, Leases: []proto.Lease{lease}})
		case "/v0/leases":
			json.NewEncoder(w).Encode([]proto.Lease{lease})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfg := config.Client{Peers: map[string]config.Peer{"cloud": {URL: srv.URL}}}
	if name, err := leaseOwnerPeer(cfg, id); err != nil || name != "cloud" {
		t.Fatalf("found %q %v", name, err)
	}
	if _, err := leaseOwnerPeer(cfg, proto.NewULID()); err == nil {
		t.Fatal("found a lease no cloud peer has")
	}
}

// A run asks the next supplier only when one turned the request down before
// starting anything; any other failure may have started a lease, so it stops.
func TestLeaseFallsBackOnlyAfterARefusal(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, tc := range []struct {
		status   int
		fallBack bool
	}{{http.StatusTooManyRequests, true}, {http.StatusForbidden, true}, {http.StatusPreconditionFailed, true}, {http.StatusInternalServerError, false}} {
		var asked atomic.Int32
		first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"no"}`, tc.status)
		}))
		second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			asked.Add(1)
			http.Error(w, `{"error":"lease limit reached"}`, http.StatusTooManyRequests)
		}))
		opt := func(name, url string) leaseOption {
			return leaseOption{Broker: placementChoice{RunCandidate: config.RunCandidate{Name: name}, Target: url}, Offer: proto.Offer{Name: "h100"}}
		}
		_, err := leaseRunner([]leaseOption{opt("cabal", first.URL), opt("mini", second.URL)}, "gpu", io.Discard)
		first.Close()
		second.Close()
		if err == nil || (asked.Load() == 1) != tc.fallBack {
			t.Fatalf("%d: asked the next supplier %d times: %v", tc.status, asked.Load(), err)
		}
	}
}

// A cloud peer lets another device in by adding its key to the leased
// login's authorized_keys over SSH, once.
func TestAdmitLeaseKeysAppendsEachKeyOnce(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	// Stands in for ssh: runs the remote command locally as the login.
	script := "#!/bin/sh\nfor last; do :; done\nHOME=" + home + " exec sh -c \"$last\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	const mac, mini = "ssh-ed25519 bWFj errand", "ssh-ed25519 bWluaQ== errand"
	target := proto.LeaseTarget{SSH: "ubuntu@box"}
	for range 2 {
		if err := admitLeaseKeys(context.Background(), target, "", []string{mac, mini}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
	if err != nil || string(got) != mac+"\n"+mini+"\n" {
		t.Fatalf("authorized_keys %q %v", got, err)
	}
	// A hand-edited file whose last line has no newline keeps that line.
	const air = "ssh-ed25519 YWly errand"
	os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte(mac), 0o600)
	if err := admitLeaseKeys(context.Background(), target, "", []string{air}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys")); string(got) != mac+"\n"+air+"\n" {
		t.Fatalf("authorized_keys %q", got)
	}
	if info, err := os.Stat(filepath.Join(home, ".ssh", "authorized_keys")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", info.Mode(), err)
	}
}
