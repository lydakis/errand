package daemon

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"github.com/lydakis/errand/internal/proto"
)

func (d *Daemon) handleStorageStats(w http.ResponseWriter, r *http.Request, id Identity) {
	var stats proto.StorageStats
	if d.namedCaches != nil {
		entries, err := d.namedCaches.Inventory(r.Context())
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		stats.NamedCaches = namedCacheStats(entries, id.Owner(), d.cfg.InsecureNoAuth)
	}
	if d.cache != nil {
		cacheStats, err := d.cache.StatsContext(r.Context())
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		stats.Cache = &cacheStats
	}

	entries, err := os.ReadDir(d.jobsDir())
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	d.mu.Lock()
	roots := make([]string, 0, len(d.jobs))
	for _, job := range d.jobs {
		if d.ownsJob(id, job) {
			roots = append(roots, job.Dir)
		}
	}
	d.mu.Unlock()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		jobID, ok := gcTombstoneJobID(entry.Name())
		if ok && d.ownsCollectedJob(id, jobID) {
			roots = append(roots, filepath.Join(d.jobsDir(), entry.Name()))
		}
	}

	for _, root := range roots {
		bytes, err := storageTreeBytes(r.Context(), root)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			// A concurrent GC may remove a receipt after the ownership snapshot.
			// Treat it as absent from this read rather than failing the fleet view.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		stats.Jobs.Items++
		stats.Jobs.Bytes += bytes
	}
	writeJSON(w, http.StatusOK, stats)
}

func storageTreeBytes(ctx context.Context, root string) (int64, error) {
	return storageTreeBytesWithInfo(ctx, root, func(entry fs.DirEntry) (fs.FileInfo, error) {
		return entry.Info()
	})
}

func storageTreeBytesWithInfo(
	ctx context.Context,
	root string,
	entryInfo func(fs.DirEntry) (fs.FileInfo, error),
) (int64, error) {
	var total int64
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
		if entry.Type().IsRegular() {
			info, err := entryInfo(entry)
			if err != nil {
				if path != root && errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}
