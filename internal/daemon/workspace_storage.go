package daemon

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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
		case "push":
			usage.TransferBytes += info.Size()
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

// Uploads live outside workspace directories so removal cannot interrupt them.
// Snapshot ownership under the metadata mutex, then inspect bytes without it.
func (s *workspaceStore) addUploadStorage(ctx context.Context, owner string, stats *proto.StorageStats, present map[string]bool) error {
	s.mu.Lock()
	var uploads []*workspaceUpload
	for _, upload := range s.uploads {
		if upload.row.Owner == owner {
			uploads = append(uploads, upload)
		}
	}
	s.mu.Unlock()
	indices := make(map[string]int)
	if stats.Details != nil {
		for i, usage := range stats.Details.Workspaces {
			indices[usage.ID] = i
		}
	}
	for _, upload := range uploads {
		bytes, err := storageTreeBytes(ctx, upload.dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		stats.Workspaces.Bytes += bytes
		if !present[upload.row.ID] {
			stats.Workspaces.Items++
			present[upload.row.ID] = true
		}
		if stats.Details == nil {
			continue
		}
		index, found := indices[upload.row.ID]
		if !found {
			index = len(stats.Details.Workspaces)
			indices[upload.row.ID] = index
			stats.Details.Workspaces = append(stats.Details.Workspaces, proto.WorkspaceStorage{ID: upload.row.ID, Name: upload.row.Name})
		}
		usage := &stats.Details.Workspaces[index]
		usage.Bytes += bytes
		usage.TransferBytes += bytes
	}
	if stats.Details != nil {
		sort.Slice(stats.Details.Workspaces, func(i, j int) bool {
			a, b := stats.Details.Workspaces[i], stats.Details.Workspaces[j]
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			return a.ID < b.ID
		})
	}
	return nil
}
