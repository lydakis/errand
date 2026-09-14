package changes

import (
	"context"
	"fmt"
	"os"

	"github.com/lydakis/errand/internal/proto"
)

func validateTransferMaterialization(manifest proto.Manifest, maxBytes int64) error {
	if _, err := transferBlobEntries(manifest); err != nil {
		return err
	}
	if maxBytes < 0 {
		return fmt.Errorf("transfer reconstruction requires a nonnegative byte limit")
	}
	var logicalBytes int64
	for _, entry := range manifest.Entries {
		if entry.Type == proto.EntryFile {
			if entry.Size > maxBytes-logicalBytes {
				return ErrByteLimitExceeded
			}
			logicalBytes += entry.Size
		}
	}
	return nil
}

// materializePrivate builds an unpublished member of a caller-owned transaction.
// The caller cleans up on failure and publishes the containing directory only
// after all members are durable. No standalone change-base name is published.
func (s TransferBlobStore) materializePrivate(ctx context.Context, dest string, m proto.Manifest, maxBytes int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateTransferMaterialization(m, maxBytes); err != nil {
		return err
	}
	storage, err := s.open()
	if err != nil {
		return err
	}
	defer storage.Close()
	if err := os.Mkdir(dest, 0700); err != nil {
		return err
	}
	tree, err := openApplyDestination(dest)
	if err != nil {
		return err
	}
	defer tree.Close()
	if err := transferStorageOutsideWorkspace(storage.root, tree.identity); err != nil {
		return err
	}
	if err := transferStorageOutsideWorkspace(tree.root, storage.identity); err != nil {
		return err
	}
	if err := materializeTransferBase(ctx, storage.root, tree.root, m); err != nil {
		return err
	}
	return verifyTransferPaths(storage, tree)
}
