package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
)

func TestWhereSelectionFiltersAndBalances(t *testing.T) {
	e := config.EffectiveRun{Where: "os=linux,go"}
	for _, name := range []string{"offline", "wrong-os", "busy", "loaded", "available"} {
		e.Candidates = append(e.Candidates, config.RunCandidate{Name: name, URL: name})
	}
	selection, err := chooseRunners(context.Background(), e, func(_ context.Context, url, where string, _ time.Duration) (proto.Info, error) {
		i := proto.Info{MaxJobs: 4, Facts: proto.Facts{OS: "linux", Tools: map[string]string{"go": "/bin/go"}}}
		switch url {
		case "offline":
			return i, fmt.Errorf("unreachable")
		case "wrong-os":
			i.Facts.OS = "darwin"
		case "busy":
			i.Busy = true
		case "loaded":
			i.StagingJobs = 3
		}
		return i, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	choices := selection.Choices
	if len(choices) != 2 || choices[0].Name != "available" {
		t.Fatalf("choices=%+v", choices)
	}
	var errOut bytes.Buffer
	selection.printExcluded(&errOut)
	for _, s := range []string{"offline", "wrong-os", "busy"} {
		if !strings.Contains(errOut.String(), s) {
			t.Fatalf("missing exclusion %s: %s", s, &errOut)
		}
	}
}

func TestWhereProbeFanoutAndCancellation(t *testing.T) {
	e := config.EffectiveRun{Where: "*"}
	for i := 0; i < 20; i++ {
		e.Candidates = append(e.Candidates, config.RunCandidate{Name: fmt.Sprint(i), URL: fmt.Sprint(i)})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var active, peak atomic.Int32
	reached := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := chooseRunners(ctx, e, func(ctx context.Context, _, _ string, _ time.Duration) (proto.Info, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			if n == 8 {
				select {
				case reached <- struct{}{}:
				default:
				}
			}
			<-ctx.Done()
			return proto.Info{}, ctx.Err()
		})
		done <- err
	}()
	select {
	case <-reached:
	case <-time.After(time.Second):
		t.Fatal("probes did not run concurrently")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled probes matched")
		}
	case <-time.After(time.Second):
		t.Fatal("probes ignored cancellation")
	}
	if peak.Load() > 8 {
		t.Fatalf("unbounded probes: %d", peak.Load())
	}
}

func TestDoctorWhereReportsChoiceWithoutSubmitting(t *testing.T) {
	writeClientConfig(t, "[peers.runner]\nurl='http://runner.invalid'\n[peers.offline]\nurl='http://offline.invalid'\n")
	t.Chdir(t.TempDir())
	var out, errOut bytes.Buffer
	var calls atomic.Int32
	code := cmdDoctorWith([]string{"--where", "go", "--json"}, &out, &errOut, doctorServices{where: func(_ context.Context, target, where string, _ time.Duration) (proto.Info, error) {
		calls.Add(1)
		if target == "http://offline.invalid" {
			return proto.Info{}, fmt.Errorf("unreachable test peer")
		}
		if target != "http://runner.invalid" || where != "go" {
			t.Errorf("probe %s %s", target, where)
		}
		return proto.Info{Version: version, MaxJobs: 1, Facts: proto.Facts{Tools: map[string]string{"go": "/bin/go"}}}, nil
	}, probe: func(context.Context, string) (proto.Info, error) {
		t.Fatal("redundant probe")
		return proto.Info{}, nil
	}})
	if code != 0 || calls.Load() != 2 || !strings.Contains(out.String(), `"peer": "runner"`) || !strings.Contains(out.String(), `"reason": "unreachable test peer"`) {
		t.Fatalf("code=%d out=%s err=%s", code, &out, &errOut)
	}
}

func TestDoctorWhereKeepsSSHDisplayURL(t *testing.T) {
	writeClientConfig(t, "[peers.runner]\nssh='host'\nremote_command='/custom/errand'\n")
	t.Chdir(t.TempDir())
	var out, errOut bytes.Buffer
	code := cmdDoctorWith([]string{"--where", "*", "--json"}, &out, &errOut, doctorServices{where: func(_ context.Context, target, _ string, _ time.Duration) (proto.Info, error) {
		if target == "ssh://host" {
			t.Error("custom SSH transport was not registered")
		}
		return proto.Info{Version: version, MaxJobs: 1}, nil
	}, ssh: func(context.Context, string) error {
		t.Fatal("repeated SSH inspection after successful info probe")
		return nil
	}})
	if code != 0 || !strings.Contains(out.String(), `"url": "ssh://host"`) {
		t.Fatalf("code=%d out=%s err=%s", code, &out, &errOut)
	}
}

// A workspace's where may pick your runners, but renting spends your money,
// so only your own config, a profile you chose or --where may lease.
func TestWorkspaceWhereNeverLeases(t *testing.T) {
	broker := func(_ context.Context, _, _ string, _ time.Duration) (proto.Info, error) {
		return proto.Info{MaxJobs: 1, Facts: proto.Facts{OS: "linux"}, Offers: []proto.Offer{{Name: "h100", Facts: proto.Facts{OS: "linux", GPUs: []proto.GPU{{Name: "H100", MemoryMiB: 81920}}}}}}, nil
	}
	e := config.EffectiveRun{Where: "gpu", Candidates: []config.RunCandidate{{Name: "cloud", URL: "cloud"}}}
	selection, err := chooseRunners(context.Background(), e, broker)
	if err == nil || len(selection.Leases) > 0 || !strings.Contains(err.Error(), "pass --where \"gpu\" to lease") {
		t.Fatalf("workspace where: %v %+v", err, selection.Leases)
	}
	e.WhereMayLease = true
	if selection, err := chooseRunners(context.Background(), e, broker); err != nil || len(selection.Leases) == 0 {
		t.Fatalf("--where: %v %+v", err, selection.Leases)
	}
}

// Lease guarantee 5: a ready lease of yours that matches is reused by asking
// its cloud peer, which hands it to the run, never by running on it
// directly; nothing would then keep it from going idle under the run.
func TestReadyLeasesAreReusedThroughTheirCloudPeer(t *testing.T) {
	writeClientConfig(t, "[peers.cloud]\nurl = \"http://cloud:7443\"\n")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	h100 := proto.Facts{OS: "linux", GPUs: []proto.GPU{{Name: "H100", MemoryMiB: 81920}}}
	var probed []string
	probe := func(_ context.Context, target, _ string, _ time.Duration) (proto.Info, error) {
		probed = append(probed, target)
		info := proto.Info{MaxJobs: 1, Facts: proto.Facts{OS: "linux"}}
		if target == "http://cloud:7443" {
			info.Offers = []proto.Offer{{Name: "h100", Facts: h100}}
			info.Leases = []proto.Lease{{ID: proto.NewULID(), Offer: "h100", State: proto.LeaseReady, Target: &proto.LeaseTarget{URL: "http://box:7443"}, Facts: &h100}}
		}
		return info, nil
	}
	e := config.EffectiveRun{Where: "gpu=h100", WhereMayLease: true, Candidates: []config.RunCandidate{{Name: "cloud", URL: "http://cloud:7443"}}}
	s, err := chooseRunners(context.Background(), e, probe)
	if err != nil || len(s.Choices) != 0 || len(s.Leases) != 1 || s.Leases[0].Broker.Name != "cloud" || len(probed) != 1 {
		t.Fatalf("selection %v %+v, probed %q", err, s, probed)
	}
}

// Every reachable cloud peer offering a match is a supplier. The cheapest
// offer comes first, priced before unpriced, and equal offers are tried in
// random order. The cloud peers' own load does not matter.
func TestLeaseSuppliersRankedByOffer(t *testing.T) {
	gpu := proto.Facts{OS: "linux", GPUs: []proto.GPU{{Name: "H100", MemoryMiB: 81920}}}
	offers := map[string][]proto.Offer{}
	probe := func(_ context.Context, target, _ string, _ time.Duration) (proto.Info, error) {
		if target == "gone" {
			return proto.Info{}, errors.New("unreachable")
		}
		busy := map[string]int{"cabal": 1}[target]
		return proto.Info{MaxJobs: 1, RunningJobs: busy, Facts: proto.Facts{OS: "linux"}, Offers: offers[target]}, nil
	}
	order := func(peers ...string) []string {
		e := config.EffectiveRun{Where: "gpu=h100", WhereMayLease: true}
		for _, p := range peers {
			e.Candidates = append(e.Candidates, config.RunCandidate{Name: p, URL: p})
		}
		s, err := chooseRunners(context.Background(), e, probe)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, o := range s.Leases {
			names = append(names, o.Broker.Name+":"+o.Offer.Name)
		}
		return names
	}
	offers["cabal"] = []proto.Offer{{Name: "lambda", Facts: gpu, PricePerHour: new(2.49)}}
	offers["mini"] = []proto.Offer{{Name: "pool", Facts: gpu}, {Name: "cheap", Facts: gpu, PricePerHour: new(1.99)}, {Name: "a10", Facts: proto.Facts{OS: "linux"}, PricePerHour: new(0.5)}}
	if got := strings.Join(order("cabal", "mini", "gone"), " "); got != "mini:cheap cabal:lambda" {
		t.Fatalf("ranked %s", got)
	}
	offers["mini"] = []proto.Offer{{Name: "pool", Facts: gpu}}
	if got := strings.Join(order("cabal", "mini"), " "); got != "cabal:lambda mini:pool" {
		t.Fatalf("an unpriced offer ranked first: %s", got)
	}
	offers["mini"] = []proto.Offer{{Name: "same", Facts: gpu, PricePerHour: new(2.49)}}
	first := map[string]bool{}
	for range 50 {
		first[order("cabal", "mini")[0]] = true
	}
	if len(first) != 2 {
		t.Fatalf("equal offers not shared: %v", first)
	}
}

// An installed local runner rents through its cloud offers when nothing of
// yours matches, but it is asked only then and never runs the job itself.
func TestLocalRunnerOnlySuppliesLeases(t *testing.T) {
	gpu := proto.Facts{OS: "linux", GPUs: []proto.GPU{{Name: "A10", MemoryMiB: 24576}}}
	var probed []string
	down := false
	probe := func(_ context.Context, target, _ string, _ time.Duration) (proto.Info, error) {
		probed = append(probed, target)
		if target == "local" {
			if down {
				return proto.Info{}, errors.New("connection refused")
			}
			// The laptop's own facts match, so only the supplier rule keeps
			// the job off it.
			return proto.Info{MaxJobs: 1, Facts: gpu, Offers: []proto.Offer{{Name: "a10", Facts: gpu}}}, nil
		}
		return proto.Info{MaxJobs: 1, Facts: proto.Facts{OS: "linux"}}, nil
	}
	local := []config.RunCandidate{{Name: "local", URL: "local"}}
	e := config.EffectiveRun{Where: "gpu", WhereMayLease: true, LeaseSuppliers: local}
	s, err := chooseRunners(context.Background(), e, probe)
	if err != nil || len(s.Choices) != 0 || len(s.Leases) != 1 || s.Leases[0].Broker.Name != "local" {
		t.Fatalf("laptop only: %v %+v", err, s)
	}
	e.Candidates = []config.RunCandidate{{Name: "box", URL: "box"}}
	if s, err := chooseRunners(context.Background(), e, probe); err != nil || len(s.Leases) != 1 || s.Leases[0].Broker.Name != "local" {
		t.Fatalf("with a non-matching runner: %v %+v", err, s)
	}
	probed = nil
	e.Where = "os=linux"
	if s, err := chooseRunners(context.Background(), e, probe); err != nil || len(s.Choices) != 1 || s.Choices[0].Name != "box" || strings.Join(probed, " ") != "box" {
		t.Fatalf("matching runner: %v %+v, probed %q", err, s, probed)
	}
	e = config.EffectiveRun{Where: "gpu", WhereMayLease: true, LeaseSuppliers: local}
	down = true
	if _, err := chooseRunners(context.Background(), e, probe); err == nil || !strings.Contains(err.Error(), "local: connection refused") {
		t.Fatalf("unreachable local runner: %v", err)
	}
	e.LeaseSuppliers = nil
	if _, err := chooseRunners(context.Background(), e, probe); err == nil || !strings.Contains(err.Error(), "add a peer first") {
		t.Fatalf("no runners: %v", err)
	}
}
