package changes

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/lydakis/errand/internal/archive"
	"github.com/lydakis/errand/internal/proto"
)

const workspaceBaseDirectory = "change-base"

func workspaceBasePath(jobDir string) string {
	return filepath.Join(jobDir, workspaceBaseDirectory)
}

// CaptureWorkspaceBaseContext preserves the submitted tree before the command
// can mutate it. Filesystems with copy-on-write cloning keep this inexpensive;
// other filesystems fall back to verified copies.
func CaptureWorkspaceBaseContext(ctx context.Context, workspace, jobDir string, manifest proto.Manifest) error {
	return captureWorkspaceBaseContext(ctx, workspace, jobDir, manifest, syncStagedData, syncDirectory)
}

func captureWorkspaceBaseContext(ctx context.Context, workspace, jobDir string, manifest proto.Manifest, syncData func(*os.File) error, syncDir func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := archive.Validate(manifest); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(jobDir, ".change-base-")
	if err != nil {
		return err
	}
	defer RemoveTree(tmp)

	source, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer source.Close()
	tree, err := os.OpenRoot(tmp)
	if err != nil {
		return err
	}
	defer tree.Close()
	policy := materializationPolicy{permissions: manifestPermissions, cloneFiles: true,
		syncData: syncData, barrier: func() error { return syncDir(tmp) }}
	if err := materializeSourceAtRoot(ctx, source, tree, manifest, nil, policy); err != nil {
		return fmt.Errorf("capturing change base: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmp, workspaceBasePath(jobDir)); err != nil {
		return err
	}
	return syncDir(jobDir)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
