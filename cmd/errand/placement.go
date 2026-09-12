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
			switch {
			case !info.Placement:
				reasons[i] = "runner does not support requirement validation; upgrade it"
			case info.Busy:
				reasons[i] = "runner is full or unavailable"
			case info.MaxJobs <= 0 || info.MaxQueued < 0 || info.RunningJobs < 0 || info.StartingJobs < 0 || info.StagingJobs < 0 || info.QueuedJobs < 0:
				reasons[i] = "invalid capacity report"
			default:
				if missing := q.Missing(info.Facts); len(missing) > 0 {
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
	var result placementSelection
	for i, c := range choices {
		if reasons[i] != "" {
			result.Excluded = append(result.Excluded, placementExclusion{Peer: e.Candidates[i].Name, Reason: reasons[i]})
			exclusions = append(exclusions, fmt.Sprintf("%s: %s", terminalSafeField(e.Candidates[i].Name), terminalSafeField(reasons[i])))
			continue
		}
		eligible = append(eligible, c)
	}
	if len(eligible) == 0 {
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

func runChoices(e config.EffectiveRun, rawURL bool, stderr io.Writer) ([]placementChoice, error) {
	if e.Where != "" {
		selection, err := chooseRunners(context.Background(), e, client.ProbeWhereInfo)
		if err == nil {
			selection.printExcluded(stderr)
		}
		return selection.Choices, err
	}
	c := placementChoice{RunCandidate: config.RunCandidate{Name: e.Peer, URL: e.URL, RemoteCommand: e.RemoteCommand, RemoteSocket: e.RemoteSocket}, Target: e.URL}
	// Raw SSH URLs must retain their identity for handle and change-state lookups.
	if !rawURL {
		c.Target = client.ConfigureSSHPeer(c.URL, c.Name, c.RemoteCommand, c.RemoteSocket)
	}
	return []placementChoice{c}, nil
}

func configurePlacement(opts *client.RunOptions, choices []placementChoice, stderr io.Writer, selected func(placementChoice)) {
	byTarget := make(map[client.RunTarget]placementChoice, len(choices))
	for _, c := range choices {
		target := client.RunTarget{PeerURL: c.Target, PeerName: c.Name}
		opts.Candidates = append(opts.Candidates, target)
		byTarget[target] = c
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
