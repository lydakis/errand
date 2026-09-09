package daemon

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

// Walk once so the summary and detail share the same observation. Like other
// storage categories these are logical regular-file bytes; symlinks are never
// followed, so named caches are not charged to the workspace as well.
func workspaceStorageBytes(ctx context.Context, root string, row workspaceRecord) (proto.WorkspaceStorage, error) {
	usage := proto.WorkspaceStorage{ID: row.ID, Name: row.Name, JobIDs: row.JobIDs}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if path != root && errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		category, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
		switch category {
		case "data":
			usage.WorkingBytes += info.Size()
		case "change-base":
			usage.BaseBytes += info.Size()
		default:
			usage.MetadataBytes += info.Size()
		}
		usage.Bytes += info.Size()
		return nil
	})
	return usage, err
}
