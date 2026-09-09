package changes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

// MaterializeBase reconstructs a private change-base using only retained bodies
// and manifest metadata. The destination must not already exist. Publication is
// atomic; missing or corrupt content never publishes a partial baseline. Files
// are independent copies, so later edits cannot mutate retained blobs. maxBytes
// bounds logical output size, including repeated references to the same blob.
func (s TransferBlobStore) MaterializeBase(ctx context.Context, jobDir string, manifest proto.Manifest, maxBytes int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	storage, err := s.open()
	if err != nil {
		return err
	}
	defer storage.Close()
	job, err := openApplyDestination(jobDir)
	if err != nil {
		return err
	}
	defer job.Close()
	if err := transferStorageOutsideWorkspace(storage.root, job.identity); err != nil {
		return err
	}
	if err := transferStorageOutsideWorkspace(job.root, storage.identity); err != nil {
		return err
	}
	if _, err := job.root.Lstat(workspaceBaseDirectory); err == nil {
		return fmt.Errorf("change base already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp := ".change-base-" + proto.NewULID()
	if err := job.root.Mkdir(tmp, 0700); err != nil {
		return err
	}
	defer removeTreeAtRoot(job.root, tmp)
	tree, err := job.root.OpenRoot(tmp)
	if err != nil {
		return err
	}
	defer tree.Close()
	if err := materializeTransferBase(ctx, storage.root, tree, manifest); err != nil {
		return err
	}
	if err := verifyTransferPaths(storage, job); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := job.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := renameNoReplace(dir, tmp, dir, workspaceBaseDirectory); err != nil {
		return err
	}
	return errors.Join(dir.Sync(), verifyTransferPaths(storage, job))
}

func materializeTransferBase(ctx context.Context, storage, tree *os.Root, manifest proto.Manifest) error {
	directories := map[string]os.FileMode{}
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
			if err := tree.Symlink(e.Target, e.Path); err != nil {
				return err
			}
		case proto.EntryFile:
			in, err := openTransferBlob(storage, strings.ToLower(e.SHA256), e)
			if err != nil {
				return err
			}
			out, err := tree.OpenFile(e.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				in.Close()
				return err
			}
			copyErr := copyTransferBlob(ctx, out, in, e)
			err = errors.Join(copyErr, out.Chmod(os.FileMode(e.Mode)), out.Sync(), out.Close(), in.Close())
			if err != nil {
				return err
			}
		}
	}
	// Sync all directory entries, including implicit parents, before restricting
	// access. Reverse lexical order visits each descendant before its ancestors.
	names := make([]string, 0, len(directories))
	for name := range directories {
		names = append(names, name)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir, err := tree.Open(name)
		if err != nil {
			return err
		}
		if err := errors.Join(dir.Chmod(directories[name]), dir.Sync(), dir.Close()); err != nil {
			return err
		}
	}
	return syncApplyRootDirectory(tree, ".")
}
