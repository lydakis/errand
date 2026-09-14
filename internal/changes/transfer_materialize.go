package changes

import (
	"context"
	"errors"
	"io"
	"os"
	"path"

	"github.com/lydakis/errand/internal/proto"
)

// materializeTransferTree builds a private tree from verified content readers.
// Blob reconstruction and incoming transfer staging use the same member flushes,
// child-first mode restoration and final durability barrier. The caller owns
// cleanup and publication; no failed tree is exposed under a durable name.
func materializeTransferTree(ctx context.Context, tree *os.Root, manifest proto.Manifest,
	open func(proto.ManifestEntry) (io.ReadCloser, error), syncData func(*os.File) error, barrier func() error,
) error {
	directories := map[string]os.FileMode{}
	var files, symlinks []proto.ManifestEntry
	for _, e := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := tree.MkdirAll(path.Dir(e.Path), 0700); err != nil {
			return err
		}
		for parent := path.Dir(e.Path); parent != "."; parent = path.Dir(parent) {
			if _, exists := directories[parent]; !exists {
				directories[parent] = 0700
			}
		}
		switch e.Type {
		case proto.EntryDir:
			directories[e.Path] = os.FileMode(e.Mode)
			if err := tree.MkdirAll(e.Path, 0700); err != nil {
				return err
			}
		case proto.EntrySymlink:
			symlinks = append(symlinks, e)
		case proto.EntryFile:
			files = append(files, e)
		}
	}
	if err := runStagingTasksContext(ctx, len(files), func(ctx context.Context, i int) error {
		e := files[i]
		in, err := open(e)
		if err != nil {
			return err
		}
		out, err := tree.OpenFile(e.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return errors.Join(err, in.Close())
		}
		copyErr := copyTransferBlob(ctx, out, in, e)
		if copyErr != nil {
			return errors.Join(copyErr, out.Close(), in.Close())
		}
		return errors.Join(out.Chmod(os.FileMode(e.Mode)), syncData(out), out.Close(), in.Close())
	}); err != nil {
		return err
	}
	// No file or implicit directory creation can traverse a link we created.
	for _, e := range symlinks {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := tree.Symlink(e.Target, e.Path); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(directories))
	for name := range directories {
		names = append(names, name)
	}
	for _, group := range childFirstPathGroups(names) {
		if err := runStagingTasksContext(ctx, len(group), func(ctx context.Context, i int) error {
			name := group[i]
			dir, err := tree.Open(name)
			if err != nil {
				return err
			}
			return errors.Join(dir.Chmod(directories[name]), syncData(dir), dir.Close())
		}); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return barrier()
}
