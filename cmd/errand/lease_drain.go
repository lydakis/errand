package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/cloud"
	"github.com/lydakis/errand/internal/proto"
)

// drainLeaseTarget has an idle leased runner refuse new jobs, through the
// hold errand setup takes before a restart, so that a release for idleness
// cannot lose a job admitted after the last idle check. The runner allows
// this only to its own user over its local socket, so the cloud peer asks
// over SSH.
func drainLeaseTarget(ctx context.Context, t proto.LeaseTarget, identity string) (func(context.Context) error, error) {
	peer, err := leaseTargetPeer(t, identity)
	if err != nil {
		return nil, err
	}
	c := leaseCandidate(leasePeer{Name: "lease", Peer: peer})
	target := client.ConfigureSSHPeer(c.URL, c.Name, c.RemoteCommand, c.RemoteSocket)
	token, err := client.QuiesceRunner(ctx, target)
	switch {
	case errors.Is(err, client.ErrRunnerNotIdle):
		return nil, fmt.Errorf("%w: %v", cloud.ErrRunnerBusy, err)
	case errors.Is(err, client.ErrQuiesceRefused):
		return nil, fmt.Errorf("%w: %v", cloud.ErrUndrainable, err)
	}
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error { return client.ResumeRunner(ctx, target, token) }, nil
}
