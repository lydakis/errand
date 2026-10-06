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
// cannot lose a job admitted after the last idle check; with renew, it
// extends that hold. The runner allows this only to its own user over its
// local socket, so the cloud peer asks over SSH.
func drainLeaseTarget(ctx context.Context, t proto.LeaseTarget, identity, renew string) (string, error) {
	target, err := leaseRunnerSocket(t, identity)
	if err != nil {
		return "", err
	}
	token, err := client.QuiesceRunner(ctx, target, renew)
	switch {
	case errors.Is(err, client.ErrRunnerNotIdle):
		return "", fmt.Errorf("%w: %v", cloud.ErrRunnerBusy, err)
	case errors.Is(err, client.ErrQuiesceRefused):
		return "", fmt.Errorf("%w: %v", cloud.ErrUndrainable, err)
	}
	return token, err
}

// resumeLeaseTarget lifts a hold drainLeaseTarget took.
func resumeLeaseTarget(ctx context.Context, t proto.LeaseTarget, identity, token string) error {
	target, err := leaseRunnerSocket(t, identity)
	if err != nil {
		return err
	}
	return client.ResumeRunner(ctx, target, token)
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
