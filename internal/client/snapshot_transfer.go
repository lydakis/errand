package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/lydakis/errand/internal/proto"
)

type shipPlan struct {
	partial bool
	hashes  map[string]bool
}

func (p shipPlan) ships(entry proto.ManifestEntry) bool {
	return !p.partial || p.hashes[entry.SHA256]
}

func negotiateSnapshot(ctx context.Context, opts RunOptions, manifest proto.Manifest) (shipPlan, error) {
	if opts.workspaceID != "" {
		return shipPlan{}, nil
	}
	return negotiateSnapshotAt(ctx, opts.PeerURL+"/v0/snapshot/diff", manifest)
}

func negotiateSnapshotAt(ctx context.Context, endpoint string, manifest proto.Manifest) (shipPlan, error) {
	refs := make([]proto.BlobRef, 0, len(manifest.Entries))
	seen := make(map[string]bool, len(manifest.Entries))
	for _, e := range manifest.Entries {
		if e.Type == proto.EntryFile && !seen[e.SHA256] {
			seen[e.SHA256] = true
			refs = append(refs, proto.BlobRef{SHA256: e.SHA256, Size: e.Size})
		}
	}
	if len(refs) == 0 {
		return shipPlan{}, nil
	}
	body, err := json.Marshal(proto.SnapshotDiffRequest{Blobs: refs})
	if err != nil {
		return shipPlan{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, controlRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return shipPlan{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := directHTTP.Do(req)
	if err != nil {
		return shipPlan{}, err
	}
	defer resp.Body.Close()
	// A cold cache can return every requested hash. Bound the response by
	// the request size instead of rejecting otherwise valid large manifests.
	// Each JSON hash takes 67 bytes (quotes and comma); allow framing space.
	maxResponseBytes := int64(len(refs))*67 + 1024
	if maxResponseBytes < 4<<20 {
		maxResponseBytes = 4 << 20
	}
	raw, err := readBoundedBody(resp.Body, maxResponseBytes, "snapshot negotiation response")
	if err != nil {
		return shipPlan{}, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound: // Snapshot caching is disabled on this runner.
		return shipPlan{}, nil
	default:
		return shipPlan{}, fmt.Errorf("snapshot negotiation: %s: %s", resp.Status, apiError(raw))
	}
	var diff proto.SnapshotDiffResponse
	if err := json.Unmarshal(raw, &diff); err != nil {
		return shipPlan{}, err
	}
	ship := make(map[string]bool, len(diff.Missing))
	for _, h := range diff.Missing {
		ship[h] = true
	}
	return shipPlan{partial: true, hashes: ship}, nil
}

var errSnapshotCacheMiss = errors.New("runner could not restore negotiated snapshot content")

// Only an explicit cache-miss rejection permits one full-upload fallback.
// Callers keep their source and transaction identity fixed across attempts.
func uploadWithSnapshotFallback(plan shipPlan, upload func(shipPlan) error, notice func()) error {
	err := upload(plan)
	if !plan.partial || !errors.Is(err, errSnapshotCacheMiss) {
		return err
	}
	if notice != nil {
		notice()
	}
	return upload(shipPlan{})
}
