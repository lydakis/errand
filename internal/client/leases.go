package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

const maxLeaseResponseBytes = 1 << 20

// AcquireLease asks a cloud peer for a machine matching where, giving each
// attempt up to timeout. The answer may be an existing lease of the caller's
// that already matches. A request whose answer is lost may still have
// started a lease, so it is sent once more under the same request ID, which
// returns that lease. If that does not settle it either, the error is one
// LeaseUncertain reports. requestID is a ULID that also names the request
// to WithdrawLeaseRequest.
func AcquireLease(ctx context.Context, peerURL, requestID, where, sshKey string, timeout time.Duration) (proto.Lease, error) {
	body, _ := json.Marshal(proto.LeaseRequest{RequestID: requestID, Where: where, SSHKey: sshKey})
	endpoint := strings.TrimSuffix(peerURL, "/") + "/v0/leases"
	var lease proto.Lease
	var err error
	for attempt := range 2 {
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		err = leaseRequest(attemptCtx, http.MethodPost, endpoint, body, &lease)
		cancel()
		if err == nil {
			return lease, nil
		}
		var answered *controlHTTPError
		if errors.As(err, &answered) {
			if attempt == 0 {
				return lease, err
			}
			// A cloud peer can refuse a retry before it looks for what the
			// first attempt started, so the refusal does not say whether
			// that attempt started anything.
			return lease, &uncertainLease{fmt.Errorf("%v, after the answer to the first attempt was lost", err)}
		}
		if ctx.Err() != nil {
			break
		}
	}
	return lease, &uncertainLease{err}
}

// uncertainLease is AcquireLease failing after an attempt that may have
// reached the cloud peer went unanswered.
type uncertainLease struct{ err error }

func (e *uncertainLease) Error() string { return e.err.Error() }

// LeaseUncertain reports whether a lease request failed without settling
// whether the cloud peer started a lease for it. Only withdrawing the
// request settles it.
func LeaseUncertain(err error) bool {
	var uncertain *uncertainLease
	return errors.As(err, &uncertain)
}

func GetLease(ctx context.Context, peerURL, id string) (proto.Lease, error) {
	var lease proto.Lease
	err := getJSONContext(ctx, strings.TrimSuffix(peerURL, "/")+"/v0/leases/"+url.PathEscape(id), maxLeaseResponseBytes, "lease", &lease)
	return lease, err
}

func ListLeases(ctx context.Context, peerURL string) ([]proto.Lease, error) {
	var leases []proto.Lease
	err := getJSONContext(ctx, strings.TrimSuffix(peerURL, "/")+"/v0/leases", 16<<20, "leases", &leases)
	return leases, err
}

func ReleaseLease(ctx context.Context, peerURL, id string) (proto.Lease, error) {
	var lease proto.Lease
	err := leaseRequest(ctx, http.MethodDelete, strings.TrimSuffix(peerURL, "/")+"/v0/leases/"+url.PathEscape(id), nil, &lease)
	return lease, err
}

// WithdrawLeaseRequest tells a cloud peer that the run that sent requestID
// gave up before its lease was ready. The cloud peer cancels the launch if
// no other run is waiting for it; a ready lease is left to its idle rule.
// The answer has no ID when the cloud peer has no lease recorded for the
// request.
func WithdrawLeaseRequest(ctx context.Context, peerURL, requestID string) (proto.Lease, error) {
	var lease proto.Lease
	err := leaseRequest(ctx, http.MethodDelete, strings.TrimSuffix(peerURL, "/")+"/v0/lease-requests/"+url.PathEscape(requestID), nil, &lease)
	var answered *controlHTTPError
	if errors.As(err, &answered) && answered.statusCode == http.StatusNotFound {
		return proto.Lease{}, nil
	}
	return lease, err
}

func leaseRequest(ctx context.Context, method, endpoint string, body []byte, dst *proto.Lease) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := directHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := readBoundedBody(resp.Body, maxLeaseResponseBytes, "lease")
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &controlHTTPError{statusCode: resp.StatusCode, err: fmt.Errorf("%s", apiError(raw))}
	}
	if err := json.Unmarshal(raw, dst); err != nil || !proto.ValidULID(dst.ID) {
		return fmt.Errorf("cloud peer returned an invalid lease")
	}
	return nil
}

// LeaseRefused reports whether a cloud peer turned a lease request down
// before starting anything: the caller may not lease there, it has no
// offers any more, nothing there matches, it is at its lease limit, or the
// request was already withdrawn. A
// refusal of a retry is not one: the first attempt may have started a
// lease.
func LeaseRefused(err error) bool {
	var refused *controlHTTPError
	if LeaseUncertain(err) || !errors.As(err, &refused) {
		return false
	}
	switch refused.statusCode {
	case http.StatusForbidden, http.StatusNotFound, http.StatusGone, http.StatusPreconditionFailed, http.StatusTooManyRequests:
		return true
	}
	return false
}

// AdmitLeaseKey asks a cloud peer to let this device into one of its
// caller's leased machines by sshKey. The answer lists the key in SSHKeys
// once the machine admits it.
func AdmitLeaseKey(ctx context.Context, peerURL, id, sshKey string) (proto.Lease, error) {
	body, _ := json.Marshal(proto.LeaseRequest{SSHKey: sshKey})
	var lease proto.Lease
	err := leaseRequest(ctx, http.MethodPost, strings.TrimSuffix(peerURL, "/")+"/v0/leases/"+url.PathEscape(id)+"/ssh-keys", body, &lease)
	return lease, err
}
