package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/placement"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/termui"
)

type placementProbe func(context.Context, string, string, time.Duration) (proto.Info, error)
type placementChoice struct {
	config.RunCandidate
	Info   proto.Info
	Target string // transport identity; RunCandidate.URL remains the configured display URL
	note   string // why it was chosen, for the run header
}

type placementExclusion struct {
	Peer   string      `json:"peer"`
	Reason string      `json:"reason"`
	info   *proto.Info // facts, when the runner answered but didn't match
}
type placementSelection struct {
	Choices  []placementChoice
	Excluded []placementExclusion
}

func (s placementSelection) printExcluded(e *termui.Stream) {
	for _, x := range s.Excluded {
		e.Detail("skipped", terminalSafeField(x.Peer)+": "+terminalSafeField(x.Reason))
	}
}

// placementNote says why a runner was chosen, for the run header:
// "matched os=darwin (cabal is linux)".
func placementNote(where string, excluded []placementExclusion) string {
	note := "matched " + where
	if where == "*" {
		note = "least busy runner"
	}
	var why []string
	for _, x := range excluded {
		// Reasons and facts come from runner responses; quote them before
		// they reach the run header.
		if x.info != nil && strings.Contains(x.Reason, "os=") && x.info.Facts.OS != "" {
			why = append(why, terminalSafeField(x.Peer)+" is "+terminalSafeField(x.info.Facts.OS))
			continue
		}
		why = append(why, terminalSafeField(x.Peer)+" skipped: "+terminalSafeField(x.Reason))
	}
	if len(why) > 0 {
		note += " (" + strings.Join(why, "; ") + ")"
	}
	return note
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
			result.Excluded = append(result.Excluded, placementExclusion{Peer: e.Candidates[i].Name, Reason: reasons[i], info: infos[i]})
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

func announcePlacement(e *termui.Stream, c placementChoice) {
	i := c.Info
	e.Detail("selected", fmt.Sprintf("%s · %d of %d slots busy · %d staging · %d queued", terminalSafeField(c.Name), i.StartingJobs+i.RunningJobs, i.MaxJobs, i.StagingJobs, i.QueuedJobs))
}

func runChoices(e config.EffectiveRun, rawURL bool, stderr *termui.Stream, verbose bool) ([]placementChoice, error) {
	if e.Where != "" {
		spin := stderr.Spin("Choosing a runner for " + stderr.B(e.Where) + "…")
		selection, err := chooseRunners(context.Background(), e, client.ProbeWhereInfo)
		spin.Stop()
		if err == nil && verbose {
			selection.printExcluded(stderr)
		}
		note := placementNote(e.Where, selection.Excluded)
		for i := range selection.Choices {
			selection.Choices[i].note = note
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

func configurePlacement(opts *client.RunOptions, choices []placementChoice, stderr *termui.Stream, selected func(placementChoice)) {
	byTarget := make(map[client.RunTarget]placementChoice, len(choices))
	for _, c := range choices {
		target := client.RunTarget{PeerURL: c.Target, PeerName: c.Name, Placement: c.note}
		opts.Candidates = append(opts.Candidates, target)
		byTarget[target] = c
	}
	where := opts.Where
	opts.OnSelected = func(target client.RunTarget) {
		c := byTarget[target]
		if where != "" {
			if opts.Display.Verbose {
				announcePlacement(stderr, c)
			}
			if !opts.Display.Quiet {
				warnKnownRunnerVersion(stderr, c.Info.Version, c.Name)
			}
		}
		selected(c)
	}
}
