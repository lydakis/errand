package changes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/lydakis/errand/internal/archive"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

// A job's change base is its baseline manifest plus the submitted body of each
// file, addressed by hash. Change collection reads only the bodies under the
// changed paths, so a job that changes nothing never reads its base. Most
// bodies stay in a shared content store (the runner's snapshot cache, pinned
// for the job); the job's private store holds a copy of the rest.

// SharedBlobs returns the path of a body another store keeps for the job.
// ok is false for any body the job's private store must hold instead.
type SharedBlobs func(sha string) (path string, ok bool)

func jobBlobPath(jobDir, sha string) string {
	return filepath.Join(workspaceBasePath(jobDir), sha)
}

// CaptureJobBaseContext copies the bodies shared does not hold from the job's
// workspace into its private store before the command can change them. A daemon
// restart never reads a job base, so the copies are not flushed.
func CaptureJobBaseContext(ctx context.Context, workspace, jobDir string, manifest proto.Manifest, shared SharedBlobs) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := archive.Validate(manifest); err != nil {
		return err
	}
	var files []proto.ManifestEntry
	seen := make(map[string]bool)
	for _, e := range manifest.Entries {
		if e.Type != proto.EntryFile || seen[e.SHA256] {
			continue
		}
		seen[e.SHA256] = true
		if shared != nil {
			if _, ok := shared(e.SHA256); ok {
				continue
			}
		}
		files = append(files, e)
	}
	if err := os.Mkdir(workspaceBasePath(jobDir), 0o700); err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, RemoveTree(workspaceBasePath(jobDir)))
		}
	}()
	source, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer source.Close()
	store, err := os.OpenRoot(workspaceBasePath(jobDir))
	if err != nil {
		return err
	}
	defer store.Close()
	paths := materializationPaths{root: source, verify: true}
	defer func() { err = errors.Join(err, paths.close()) }()
	if err := runStagingTasksContext(ctx, len(files), func(ctx context.Context, i int) error {
		e := files[i]
		in, err := openMaterializationSource(&paths, e, e.Mode)
		if err != nil {
			return err
		}
		out, err := materializeFile(ctx, store, e.SHA256, e, in, true)
		if err != nil {
			return errors.Join(err, in.Close())
		}
		return errors.Join(out.Close(), in.Close())
	}); err != nil {
		return fmt.Errorf("capturing change base: %w", err)
	}
	return nil
}

// jobBundleBase packs a bundle's base archive from the job's change base.
func jobBundleBase(jobDir string, shared SharedBlobs) bundleBase {
	return func(ctx context.Context, dir string, m proto.Manifest) error {
		return packBundleContentArchive(ctx, dir, baseArchiveFile, m, func(e proto.ManifestEntry) (io.ReadCloser, error) {
			if shared != nil {
				if path, ok := shared(e.SHA256); ok {
					return os.Open(path)
				}
			}
			return os.Open(jobBlobPath(jobDir, e.SHA256))
		})
	}
}

func packBundleContentArchive(ctx context.Context, dir, name string, m proto.Manifest, open func(proto.ManifestEntry) (io.ReadCloser, error)) error {
	return writeBundleArchive(ctx, dir, name, m, func(w io.Writer) error {
		return snapshot.PackContent(ctx, w, m, open)
	})
}
