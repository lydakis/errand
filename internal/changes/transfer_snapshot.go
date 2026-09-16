package changes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

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
	packErr := <-done
	// The two ends can independently reject the same changing source bytes.
	// Only translate a pure content mismatch; never discard a destination I/O
	// failure or a separate pack failure in favor of unlimited source retries.
	if _, mismatch := err.(*archive.ContentMismatchError); mismatch &&
		(packErr == nil || packErr == err || snapshot.IsSourceChanged(packErr)) {
		return fmt.Errorf("%v: %w", err, snapshot.ErrSourceChanged)
	}
	err = errors.Join(err, packErr)
	if err != nil {
		return err
	}
	return SyncTransferSource(destination, m)
}

// SyncTransferSource is for private staging only, never a concurrently used tree.
func SyncTransferSource(root string, m proto.Manifest) error {
	return syncTransferSource(root, m, syncStagedData, syncStagingBarrier)
}

func syncTransferSource(root string, m proto.Manifest, syncData, publish func(*os.File) error) error {
	access, err := makeManifestAccessibleContext(context.Background(), root, m)
	if err != nil {
		return err
	}
	// Keep the descriptor open while final modes (including a restricted root)
	// are restored. The full flush follows all member fsyncs and mode updates.
	directory, err := access.root.Open(".")
	if err != nil {
		return errors.Join(err, access.restore())
	}
	defer directory.Close()
	if err := access.restoreWithSync(syncData); err != nil {
		return err
	}
	return publish(directory)
}
