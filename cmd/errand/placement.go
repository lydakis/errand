package main

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
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
	Lease *leaseOption
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
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	choices := make([]placementChoice, len(e.Candidates))
	reasons := make([]string, len(e.Candidates))
	infos := make([]*proto.Info, len(e.Candidates))
	// matched records runners whose facts match even when they are full: a
	// busy runner of your own never causes a lease.
	matched := make([]bool, len(e.Candidates))
	var wg sync.WaitGroup
	// Bound transport/process fan-out while all probes share one deadline.
	slots := make(chan struct{}, 8)
	for i, c := range e.Candidates {
		wg.Add(1)
		go func(i int, c config.RunCandidate) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				reasons[i] = "probe deadline exceeded"
				return
			}
			target := client.ConfigureSSHPeer(c.URL, c.Name, c.RemoteCommand, c.RemoteSocket)
			info, err := probe(ctx, target, e.Where, 2*time.Second)
			if err != nil {
				reasons[i] = err.Error()
				return
			}
			infos[i] = &info
			missing := q.Missing(info.Facts)
			matched[i] = info.Placement && len(missing) == 0
			switch {
			case !info.Placement:
				reasons[i] = "runner does not support requirement validation; upgrade it"
			case info.Busy:
				reasons[i] = "runner is full or unavailable"
			case info.MaxJobs <= 0 || info.MaxQueued < 0 || info.RunningJobs < 0 || info.StartingJobs < 0 || info.StagingJobs < 0 || info.QueuedJobs < 0:
				reasons[i] = "invalid capacity report"
			default:
				if len(missing) > 0 {
					reasons[i] = strings.Join(missing, "; ")
					return
				}
				choices[i] = placementChoice{RunCandidate: c, Info: info, Target: target}
			}
		}(i, c)
	}
	wg.Wait()
	var eligible []placementChoice
	var exclusions []string
	result := placementSelection{Probed: map[string]proto.Info{}}
	for i, info := range infos {
		if info != nil {
			result.Probed[e.Candidates[i].Name] = *info
		}
	}
	for i, c := range choices {
		if reasons[i] != "" {
			result.Excluded = append(result.Excluded, placementExclusion{Peer: e.Candidates[i].Name, Reason: reasons[i]})
			exclusions = append(exclusions, fmt.Sprintf("%s: %s", terminalSafeField(e.Candidates[i].Name), terminalSafeField(reasons[i])))
			continue
		}
		eligible = append(eligible, c)
	}
	if len(eligible) == 0 {
		// Renting is only for capabilities none of your runners has, and
		// never for the bare wildcard.
		if !slices.Contains(matched, true) && !q.Any() {
			for i, info := range infos {
				if info == nil {
					continue
				}
				if offer, ok := matchingOffer(q, info.Offers); ok {
					c := e.Candidates[i]
					if !e.WhereMayLease {
						return result, fmt.Errorf("no runner matches %q (%s); %s could lease %s, but a workspace's where never rents a machine: pass --where %q to lease", e.Where, strings.Join(exclusions, "; "), terminalSafeField(c.Name), terminalSafeField(describeOffer(offer)), e.Where)
					}
					target := client.ConfigureSSHPeer(c.URL, c.Name, c.RemoteCommand, c.RemoteSocket)
					result.Lease = &leaseOption{Broker: placementChoice{RunCandidate: c, Info: *info, Target: target}, Offer: offer}
					return result, nil
				}
			}
		}
		return result, fmt.Errorf("no runner matches %q: %s", e.Where, strings.Join(exclusions, "; "))
	}
	rand.Shuffle(len(eligible), func(i, j int) { eligible[i], eligible[j] = eligible[j], eligible[i] })
	sort.SliceStable(eligible, func(i, j int) bool { return placement.LessLoaded(eligible[i].Info, eligible[j].Info) })
	result.Choices = eligible
	return result, nil
}

func announcePlacement(w io.Writer, c placementChoice, where string) {
	i := c.Info
	fmt.Fprintf(w, "errand: selected %s for %s (%d/%d slots, %d staging, %d queued)\n", terminalSafeField(c.Name), terminalSafeField(where), i.StartingJobs+i.RunningJobs, i.MaxJobs, i.StagingJobs, i.QueuedJobs)
}

// runChoices places a run. When only a lease fits, it returns no choices and
// a lease function instead, so the caller can rent the machine as late as
// possible.
func runChoices(e config.EffectiveRun, rawURL bool, stderr io.Writer) ([]placementChoice, func() (placementChoice, error), error) {
	if e.Where != "" {
		selection, err := chooseRunners(context.Background(), e, client.ProbeWhereInfo)
		// Ended leases are forgotten rather than reported as unreachable. An
		// ended lease's machine may still answer, so placement runs again
		// without them.
		if forgotten := forgetEndedLeases(selection.Probed); len(forgotten) > 0 {
			e.Candidates = slices.DeleteFunc(slices.Clone(e.Candidates), func(c config.RunCandidate) bool { return forgotten[c.Name] })
			selection, err = chooseRunners(context.Background(), e, client.ProbeWhereInfo)
		}
		if err != nil {
			return nil, nil, err
		}
		selection.printExcluded(stderr)
		if option := selection.Lease; option != nil {
			return nil, func() (placementChoice, error) { return leaseRunner(*option, e.Where, stderr) }, nil
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
func configurePlacement(opts *client.RunOptions, choices []placementChoice, lease func() (placementChoice, error), stderr io.Writer, selected func(placementChoice)) {
	byTarget := make(map[client.RunTarget]placementChoice, len(choices))
	add := func(c placementChoice) client.RunTarget {
		target := client.RunTarget{PeerURL: c.Target, PeerName: c.Name}
		byTarget[target] = c
		return target
	}
	for _, c := range choices {
		opts.Candidates = append(opts.Candidates, add(c))
	}
	if lease != nil {
		opts.Resolve = func() ([]client.RunTarget, error) {
			c, err := lease()
			if err != nil {
				return nil, err
			}
			return []client.RunTarget{add(c)}, nil
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
