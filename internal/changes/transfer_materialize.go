package changes

import (
	"context"
	"errors"
	"io"
	"os"
	"path"

	"github.com/lydakis/errand/internal/proto"
)

// Scratch inputs retain owner access; their manifests carry logical modes.
// Published staging restores manifest modes before its durability barrier.
type treePermissions uint8

const (
	manifestPermissions treePermissions = iota
	mergeInputPermissions
)

// materializationPolicy keeps the permission and durability choices together.
// Clone support is optional; every path verifies the resulting bytes before sync.
type materializationPolicy struct {
	permissions treePermissions
	cloneFiles  bool
	syncData    func(*os.File) error
	barrier     func() error
}

type materializedDirectory struct {
	mode     os.FileMode
	explicit bool
}

func durableMaterialization(barrier func() error) materializationPolicy {
	return materializationPolicy{permissions: manifestPermissions, syncData: syncStagedData, barrier: barrier}
}

func scratchMaterialization() materializationPolicy {
	return materializationPolicy{permissions: mergeInputPermissions,
		syncData: func(*os.File) error { return nil }, barrier: func() error { return nil }}
}

// materializeTransferTree builds a private tree from verified content readers.
// Blob reconstruction and incoming transfer staging use the same member flushes,
// child-first mode restoration and final durability barrier. The caller owns
// cleanup and publication; no failed tree is exposed under a durable name.
func materializeTransferTree(ctx context.Context, tree *os.Root, manifest proto.Manifest, policy materializationPolicy,
	open func(proto.ManifestEntry) (io.ReadCloser, error),
) (err error) {
	paths := materializationPaths{root: tree}
	defer func() { err = errors.Join(err, paths.close()) }()
	var directories map[string]materializedDirectory
	if policy.permissions == manifestPermissions {
		directories = make(map[string]materializedDirectory)
	}
	var files, symlinks []proto.ManifestEntry
	lastParent := "."
	for _, e := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		parent := path.Dir(e.Path)
		if parent != lastParent {
			if err := paths.mkdirAll(parent); err != nil {
				return err
			}
			lastParent = parent
		}
		for parent := path.Dir(e.Path); directories != nil && parent != "."; parent = path.Dir(parent) {
			if _, exists := directories[parent]; !exists {
				directories[parent] = materializedDirectory{}
			}
		}
		switch e.Type {
		case proto.EntryDir:
			if directories != nil {
				directories[e.Path] = materializedDirectory{mode: os.FileMode(e.Mode), explicit: true}
			}
			if err := paths.mkdirAll(e.Path); err != nil {
				return err
			}
			lastParent = e.Path
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
		parent, name, lease, err := paths.parent(e.Path)
		if err != nil {
			return errors.Join(err, in.Close())
		}
		defer paths.release(lease)
		out, err := materializeFile(ctx, parent, name, e, in, policy.cloneFiles)
		if err != nil {
			return errors.Join(err, in.Close())
		}
		var modeErr error
		if policy.permissions == manifestPermissions {
			modeErr = out.Chmod(os.FileMode(e.Mode))
		}
		return errors.Join(modeErr, policy.syncData(out), out.Close(), in.Close())
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
	if err := finalizeMaterializedDirectories(ctx, &paths, directories, policy.syncData); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return policy.barrier()
}

func finalizeMaterializedDirectories(ctx context.Context, paths *materializationPaths, directories map[string]materializedDirectory, syncData func(*os.File) error) error {
	names := make([]string, 0, len(directories))
	for name := range directories {
		names = append(names, name)
	}
	for _, group := range childFirstPathGroups(names) {
		if err := runStagingTasksContext(ctx, len(group), func(ctx context.Context, i int) error {
			name := group[i]
			dir, err := paths.open(name)
			if err != nil {
				return err
			}
			var modeErr error
			// Implicit parents already have their creation permissions. Reapplying
			// 0700 can overwrite an explicit alias's mode on case-folding filesystems.
			if entry := directories[name]; entry.explicit {
				modeErr = dir.Chmod(entry.mode)
			}
			return errors.Join(modeErr, syncData(dir), dir.Close())
		}); err != nil {
			return err
		}
	}
	return nil
}

// Cloning retains filesystem sharing without sharing mutable file identity.
// Hash the clone itself; fallback copies hash in flight, with no second read.
func materializeFile(ctx context.Context, tree *os.Root, name string, e proto.ManifestEntry, in io.Reader, clone bool) (*os.File, error) {
	if clone {
		if source, ok := in.(interface {
			cloneTo(*os.Root, string) (*os.File, error)
		}); ok {
			if out, err := source.cloneTo(tree, name); err == nil {
				if err := copyTransferBlob(ctx, io.Discard, out, e); err != nil {
					return nil, errors.Join(err, out.Close())
				}
				return out, nil
			}
		}
	}
	out, err := tree.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if err := copyTransferBlob(ctx, out, in, e); err != nil {
		return nil, errors.Join(err, out.Close())
	}
	return out, nil
}
