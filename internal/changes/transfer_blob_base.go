package changes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	if err := validateTransferMaterialization(manifest, maxBytes); err != nil {
		return err
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
	return errors.Join(syncStagingBarrier(dir), verifyTransferPaths(storage, job))
}

func materializeTransferBase(ctx context.Context, storage, tree *os.Root, manifest proto.Manifest) error {
	return materializeTransferTree(ctx, tree, manifest, durableMaterialization(func() error { return syncApplyRootDirectory(tree, ".") }), func(e proto.ManifestEntry) (io.ReadCloser, error) {
		return openTransferBlob(storage, strings.ToLower(e.SHA256), e)
	})
}
