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
// cannot lose a job admitted after the last idle check. token names the
// hold, which renews it while it lasts. The runner allows this only to its
// own user over its local socket, so the cloud peer asks over SSH.
func drainLeaseTarget(ctx context.Context, t proto.LeaseTarget, identity, token string) error {
	target, err := leaseRunnerSocket(t, identity)
	if err != nil {
		return err
	}
	return holdRunner(ctx, target, token)
}

// holdRunner has the runner at peerURL take, or renew, the hold named by
// token. A runner that holds under any other token gives a hold the cloud
// peer can neither renew nor lift by its own, which is no hold to it: that
// hold is given back and the drain fails.
func holdRunner(ctx context.Context, peerURL, token string) error {
	got, err := client.QuiesceRunner(ctx, peerURL, token)
	switch {
	case errors.Is(err, client.ErrRunnerNotIdle):
		return fmt.Errorf("%w: %v", cloud.ErrRunnerBusy, err)
	case errors.Is(err, client.ErrQuiesceRefused):
		return fmt.Errorf("%w: %v", cloud.ErrUndrainable, err)
	case err != nil:
		return err
	case got != token:
		if err := client.ResumeRunner(ctx, peerURL, got); err != nil && !errors.Is(err, client.ErrNotHeld) {
			return fmt.Errorf("runner held under token %s instead of %s, and could not lift it: %w", got, token, err)
		}
		return fmt.Errorf("runner held under token %s instead of %s", got, token)
	}
	return nil
}

// resumeLeaseTarget lifts a hold drainLeaseTarget took; a runner without
// it, because it lapsed or was never taken, has nothing to lift.
func resumeLeaseTarget(ctx context.Context, t proto.LeaseTarget, identity, token string) error {
	target, err := leaseRunnerSocket(t, identity)
	if err != nil {
		return err
	}
	if err := client.ResumeRunner(ctx, target, token); err != nil && !errors.Is(err, client.ErrNotHeld) {
		return err
	}
	return nil
}

// leaseRunnerSocket is the address of a leased runner's local socket over
// SSH.
func leaseRunnerSocket(t proto.LeaseTarget, identity string) (string, error) {
	peer, err := leaseTargetPeer(t, identity)
	if err != nil {
		return "", err
	}
	c := leaseCandidate(leasePeer{Name: "lease", Peer: peer})
	return client.ConfigureSSHPeer(c.URL, c.Name, c.RemoteCommand, c.RemoteSocket), nil
}
