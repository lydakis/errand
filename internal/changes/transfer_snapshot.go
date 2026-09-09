package changes

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/lydakis/errand/internal/archive"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

// CopyTransferSource freezes a verified snapshot without widening permissions in
// a live source tree. Destination must be a new, private directory.
func CopyTransferSource(ctx context.Context, source, destination string, m proto.Manifest, max int64) error {
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { err := snapshot.PackContext(ctx, pw, source, m); pw.CloseWithError(err); done <- err }()
	err := archive.Extract(pr, destination, m, max)
	pr.CloseWithError(err)
	err = errors.Join(err, <-done)
	if err != nil {
		return err
	}
	return SyncTransferSource(destination, m)
}

// SyncTransferSource is for private staging only, never a concurrently used tree.
func SyncTransferSource(root string, m proto.Manifest) error {
	access, err := makeManifestAccessibleContext(context.Background(), root, m)
	if err != nil {
		return err
	}
	syncErr := func() error {
		for _, e := range m.Entries {
			if e.Type == proto.EntrySymlink {
				continue
			}
			f, err := os.Open(filepath.Join(root, filepath.FromSlash(e.Path)))
			if err != nil {
				return err
			}
			err = errors.Join(f.Sync(), f.Close())
			if err != nil {
				return err
			}
		}
		return syncDirectory(root)
	}()
	return errors.Join(syncErr, access.restore())
}
