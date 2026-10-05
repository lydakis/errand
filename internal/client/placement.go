package client

import (
	"errors"
	"net/http"
)

func placementRejection(err error) bool {
	var refusal *placementRefusal
	if errors.As(err, &refusal) {
		return true
	}
	var rejected *submitHTTPError
	return errors.As(err, &rejected) && (rejected.statusCode == http.StatusTooManyRequests || rejected.statusCode == http.StatusPreconditionFailed)
}

// placementRefusal is a known requirement rejection before workspace publication.
type placementRefusal struct{ error }

// RunTarget separates submission identity from display metadata owned by the CLI.
// Claim, when set, is the run's hold on a machine resolved for it alone.
type RunTarget struct {
	PeerURL, PeerName string
	Claim             *Claim
}

// Only callers which proved a pre-admission rejection may request another
// candidate. An uncertain submission must retain its original peer and handle.
func tryCandidates[T any](opts RunOptions, attempt func(RunOptions) (T, bool)) T {
	targets := opts.Candidates
	if len(targets) == 0 {
		targets = []RunTarget{{PeerURL: opts.PeerURL, PeerName: opts.PeerName}}
	}
	// A target whose attempt never settled its claim, or that was never
	// tried, admitted nothing.
	defer func() {
		for _, target := range targets {
			target.Claim.settle(notAdmitted)
		}
	}()
	var result T
	for _, target := range targets {
		opts.PeerURL, opts.PeerName, opts.claim = target.PeerURL, target.PeerName, target.Claim
		if opts.OnSelected != nil {
			opts.OnSelected(target)
		}
		var retry bool
		result, retry = attempt(opts)
		if !retry || opts.Where == "" {
			break
		}
	}
	return result
}
