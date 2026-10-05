package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

const maxLeaseResponseBytes = 1 << 20

// AcquireLease asks a cloud peer for a machine matching where. The answer may
// be an existing lease of the caller's that already matches.
func AcquireLease(ctx context.Context, peerURL, where string) (proto.Lease, error) {
	body, _ := json.Marshal(proto.LeaseRequest{Where: where})
	var lease proto.Lease
	err := leaseRequest(ctx, http.MethodPost, strings.TrimSuffix(peerURL, "/")+"/v0/leases", body, &lease)
	return lease, err
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
