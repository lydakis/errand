package main

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/placement"
	"github.com/lydakis/errand/internal/proto"
)

type placementProbe func(context.Context, string, string, time.Duration) (proto.Info, error)
type placementChoice struct {
	config.RunCandidate
	Info   proto.Info
	Target string // transport identity; RunCandidate.URL remains the configured display URL
}

type placementExclusion struct {
	Peer   string `json:"peer"`
	Reason string `json:"reason"`
}
type placementSelection struct {
	Choices  []placementChoice
	Excluded []placementExclusion
	// Lease is set instead of Choices when no runner of the caller's matches
	// the requirements but a cloud peer offers a machine that does.
	Leases []leaseOption
	// Probed holds every candidate that answered, by name.
	Probed map[string]proto.Info
}

func (s placementSelection) printExcluded(w io.Writer) {
	for _, e := range s.Excluded {
		fmt.Fprintf(w, "errand: where skipped %s: %s\n", terminalSafeField(e.Peer), terminalSafeField(e.Reason))
	}
}

func chooseRunners(ctx context.Context, e config.EffectiveRun, probe placementProbe) (placementSelection, error) {
	q, err := placement.Parse(e.Where)
	if err != nil {
		return placementSelection{}, err
	}
	candidates := e.Candidates
	// A ready lease of yours is not a candidate here: it is reused by asking
	// its cloud peer, which hands it to this run like a new lease.
	probed := probeCandidates(ctx, candidates, e.Where, q, probe)
	var eligible []placementChoice
	var exclusions []string
	result := placementSelection{Probed: map[string]proto.Info{}}
	// matched records runners whose facts match even when they are full: a
	// busy runner of your own never causes a lease.
	matched := false
	for i, p := range probed {
		name := candidates[i].Name
		if p.info != nil {
			result.Probed[name] = *p.info
		}
		matched = matched || p.matched
		if p.reason != "" {
			result.Excluded = append(result.Excluded, placementExclusion{Peer: name, Reason: p.reason})
			exclusions = append(exclusions, fmt.Sprintf("%s: %s", terminalSafeField(name), terminalSafeField(p.reason)))
			continue
		}
		eligible = append(eligible, p.choice)
	}
	if len(eligible) == 0 {
		// Renting is only for capabilities none of your runners has, and
		// never for the bare wildcard.
		// Each reachable cloud peer offering a match is a supplier: its
		// cheapest matching offer competes with the others' on price, and
		// equal ones are tried in random order. How busy a cloud peer's own
		// runner is does not matter; the job runs on the rented machine.
		if !matched && !q.Any() {
			for i, p := range probed {
				if p.info == nil {
					continue
				}
				if offer, ok := matchingOffer(q, p.info.Offers); ok {
					c := candidates[i]
					target := client.ConfigureSSHPeer(c.URL, c.Name, c.RemoteCommand, c.RemoteSocket)
					result.Leases = append(result.Leases, leaseOption{Broker: placementChoice{RunCandidate: c, Info: *p.info, Target: target}, Offer: offer})
				}
			}
			if len(result.Leases) > 0 {
				leases := result.Leases
				rand.Shuffle(len(leases), func(i, j int) { leases[i], leases[j] = leases[j], leases[i] })
				sort.SliceStable(leases, func(i, j int) bool { return placement.CheaperOffer(leases[i].Offer, leases[j].Offer) })
				if !e.WhereMayLease {
					o := leases[0]
					result.Leases = nil
					return result, fmt.Errorf("no runner matches %q (%s); %s could lease %s, but a workspace's where never rents a machine: pass --where %q to lease", e.Where, strings.Join(exclusions, "; "), terminalSafeField(o.Broker.Name), terminalSafeField(describeOffer(o.Offer)), e.Where)
				}
				return result, nil
			}
		}
		return result, fmt.Errorf("no runner matches %q: %s", e.Where, strings.Join(exclusions, "; "))
	}
	rand.Shuffle(len(eligible), func(i, j int) { eligible[i], eligible[j] = eligible[j], eligible[i] })
	sort.SliceStable(eligible, func(i, j int) bool { return placement.LessLoaded(eligible[i].Info, eligible[j].Info) })
	result.Choices = eligible
	return result, nil
}

type candidateProbe struct {
	choice  placementChoice
	reason  string
	info    *proto.Info
	matched bool
}

// probeCandidates asks each candidate whether it can take the run, all
// sharing one deadline.
func probeCandidates(ctx context.Context, candidates []config.RunCandidate, where string, q placement.Requirements, probe placementProbe) []candidateProbe {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out := make([]candidateProbe, len(candidates))
	var wg sync.WaitGroup
	// Bound transport/process fan-out while all probes share one deadline.
	slots := make(chan struct{}, 8)
	for i, c := range candidates {
		wg.Add(1)
		go func(r *candidateProbe, c config.RunCandidate) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				r.reason = "probe deadline exceeded"
				return
			}
			target := client.ConfigureSSHPeer(c.URL, c.Name, c.RemoteCommand, c.RemoteSocket)
			info, err := probe(ctx, target, where, 2*time.Second)
			if err != nil {
				r.reason = err.Error()
				return
			}
			r.info = &info
			missing := q.Missing(info.Facts)
			r.matched = info.Placement && len(missing) == 0
			switch {
			case !info.Placement:
				r.reason = "runner does not support requirement validation; upgrade it"
			case info.Busy:
				r.reason = "runner is full or unavailable"
			case info.MaxJobs <= 0 || info.MaxQueued < 0 || info.RunningJobs < 0 || info.StartingJobs < 0 || info.StagingJobs < 0 || info.QueuedJobs < 0:
				r.reason = "invalid capacity report"
			case len(missing) > 0:
				r.reason = strings.Join(missing, "; ")
			default:
				r.choice = placementChoice{RunCandidate: c, Info: info, Target: target}
			}
		}(&out[i], c)
	}
	wg.Wait()
	return out
}

func announcePlacement(w io.Writer, c placementChoice, where string) {
	i := c.Info
	fmt.Fprintf(w, "errand: selected %s for %s (%d/%d slots, %d staging, %d queued)\n", terminalSafeField(c.Name), terminalSafeField(where), i.StartingJobs+i.RunningJobs, i.MaxJobs, i.StagingJobs, i.QueuedJobs)
}

// runChoices places a run. When only a lease fits, it returns no choices and
// a lease function instead, so the caller can rent the machine as late as
// possible.
func runChoices(e config.EffectiveRun, rawURL bool, stderr io.Writer) ([]placementChoice, func() (placementChoice, *client.Claim, error), error) {
	if e.Where != "" {
		selection, err := chooseRunners(context.Background(), e, client.ProbeWhereInfo)
		if err != nil {
			return nil, nil, err
		}
		selection.printExcluded(stderr)
		if options := selection.Leases; len(options) > 0 {
			return nil, func() (placementChoice, *client.Claim, error) { return leaseRunner(options, e.Where, stderr) }, nil
		}
		return selection.Choices, nil, nil
	}
	c := placementChoice{RunCandidate: config.RunCandidate{Name: e.Peer, URL: e.URL, RemoteCommand: e.RemoteCommand, RemoteSocket: e.RemoteSocket}, Target: e.URL}
	// Raw SSH URLs must retain their identity for handle and change-state lookups.
	if !rawURL {
		c.Target = client.ConfigureSSHPeer(c.URL, c.Name, c.RemoteCommand, c.RemoteSocket)
	}
	return []placementChoice{c}, nil, nil
}

// configurePlacement hands the choices to a run. A lease is acquired only
// once the run's local preparation has succeeded.
func configurePlacement(opts *client.RunOptions, choices []placementChoice, lease func() (placementChoice, *client.Claim, error), stderr io.Writer, selected func(placementChoice)) {
	byTarget := make(map[client.RunTarget]placementChoice, len(choices))
	add := func(c placementChoice, claim *client.Claim) client.RunTarget {
		target := client.RunTarget{PeerURL: c.Target, PeerName: c.Name, Claim: claim}
		byTarget[target] = c
		return target
	}
	for _, c := range choices {
		opts.Candidates = append(opts.Candidates, add(c, nil))
	}
	if lease != nil {
		opts.Resolve = func() ([]client.RunTarget, error) {
			c, claim, err := lease()
			if err != nil {
				return nil, err
			}
			return []client.RunTarget{add(c, claim)}, nil
		}
	}
	where := opts.Where
	opts.OnSelected = func(target client.RunTarget) {
		c := byTarget[target]
		if where != "" {
			announcePlacement(stderr, c, where)
			warnKnownRunnerVersion(c.Info.Version, c.Name)
		}
		selected(c)
	}
}
