package daemon

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"path/filepath"

	"github.com/lydakis/errand/internal/archive"
	"github.com/lydakis/errand/internal/proto"
)

// This endpoint also establishes that the runner can reconstruct partial push
// archives. Older runners return 404 and clients keep using complete uploads.
func (d *Daemon) handleWorkspacePushDiff(w http.ResponseWriter, r *http.Request, id Identity) {
	if _, err := d.pushWorkspace(r, id); err != nil {
		workspaceHTTPError(w, err)
		return
	}
	d.handleSnapshotDiff(w, r, id)
}

// Cache only verified, private upload trees, before publication or execution.
// This is the existing evictable snapshot cache, not the mutable workspace or
// named dependency caches. Failure to retain an optimization is non-fatal.
func (d *Daemon) cacheWorkspaceSource(ctx context.Context, source string, manifest proto.Manifest, restored map[string]bool) {
	if err := d.cacheSnapshotSource(ctx, source, manifest, restored); err != nil && ctx.Err() == nil {
		log.Printf("workspace snapshot cache insertion: %v", err)
	}
}

// Jobs and pushes reconstruct only verified content in private staging trees.
// Keep their cache-hit tracking and subsequent ingestion policy together.
func (d *Daemon) snapshotExtractOptions(ctx context.Context) (archive.ExtractOptions, map[string]bool) {
	var opts archive.ExtractOptions
	restored := make(map[string]bool)
	if d.cache != nil {
		opts.ResolveMissing = func(dest string, entry proto.ManifestEntry) (bool, error) {
			hit, err := d.cache.Materialize(ctx, dest, entry)
			if hit {
				restored[entry.Path] = true
			}
			return hit, err
		}
	}
	return opts, restored
}

func (d *Daemon) cacheSnapshotSource(ctx context.Context, source string, manifest proto.Manifest, restored map[string]bool) error {
	if d.cache == nil {
		return nil
	}
	seen := make(map[string]bool)
	for _, e := range manifest.Entries {
		if restored[e.Path] {
			seen[e.SHA256] = true
		}
	}
	var firstErr error
	for _, e := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Type != proto.EntryFile || seen[e.SHA256] {
			continue
		}
		if err := d.cache.Insert(ctx, filepath.Join(source, filepath.FromSlash(e.Path)), e.SHA256, e.Size); err != nil {
			// One unreadable source must not prevent caching later files.
			// Return one diagnostic rather than accumulating errors per file.
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", e.Path, err)
			}
			continue
		}
		seen[e.SHA256] = true
	}
	return firstErr
}
