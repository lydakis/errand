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
// returns that lease. requestID is a ULID that also names the request to
// WithdrawLeaseRequest.
func AcquireLease(ctx context.Context, peerURL, requestID, where, sshKey string, timeout time.Duration) (proto.Lease, error) {
	body, _ := json.Marshal(proto.LeaseRequest{RequestID: requestID, Where: where, SSHKey: sshKey})
	endpoint := strings.TrimSuffix(peerURL, "/") + "/v0/leases"
	var lease proto.Lease
	var err error
	for range 2 {
		attempt, cancel := context.WithTimeout(ctx, timeout)
		err = leaseRequest(attempt, http.MethodPost, endpoint, body, &lease)
		cancel()
		var refused *controlHTTPError
		if err == nil || errors.As(err, &refused) {
			return lease, err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return lease, fmt.Errorf("%w (if the cloud peer started a lease anyway, errand leases lists it, and it ends once idle)", err)
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
func WithdrawLeaseRequest(ctx context.Context, peerURL, requestID string) (proto.Lease, error) {
	var lease proto.Lease
	err := leaseRequest(ctx, http.MethodDelete, strings.TrimSuffix(peerURL, "/")+"/v0/lease-requests/"+url.PathEscape(requestID), nil, &lease)
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
// before starting anything: the caller may not lease there, nothing there
// matches, or it is at its lease limit.
func LeaseRefused(err error) bool {
	var refused *controlHTTPError
	if !errors.As(err, &refused) {
		return false
	}
	switch refused.statusCode {
	case http.StatusForbidden, http.StatusPreconditionFailed, http.StatusTooManyRequests:
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
