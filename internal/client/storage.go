package client

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"

	"github.com/lydakis/errand/internal/proto"
)

// ChangeStorageStats reports the current account's fetched-change storage.
// Only aggregate usage and an opaque root identity leave this process.
func ChangeStorageStats(ctx context.Context) (proto.ChangeStorageStats, error) {
	usage, err := changeStatsContext(ctx)
	if err != nil {
		return proto.ChangeStorageStats{}, err
	}
	root, err := localChangeRoot()
	if err != nil {
		return proto.ChangeStorageStats{}, err
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	return proto.ChangeStorageStats{StorageCategory: usage, StoreID: fmt.Sprintf("%x", sum)}, nil
}
