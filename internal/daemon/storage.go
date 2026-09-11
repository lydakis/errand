package daemon

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	"github.com/lydakis/errand/internal/proto"
)

func (d *Daemon) handleStorageStats(w http.ResponseWriter, r *http.Request, id Identity) {
	stats := proto.StorageStats{Changes: &proto.ChangeStorageStats{}}
	workspaceCacheLeases := make(map[string]workspaceRecord)
	workspaceJobs := make(map[string]workspaceRecord)
	if r.URL.Query().Get("verbose") == "1" {
		stats.Details = &proto.StorageDetails{Workspaces: []proto.WorkspaceStorage{}, NamedCaches: []proto.NamedCacheStorage{}, Jobs: []proto.JobStorage{}}
	}
	if d.workspaces != nil {
		d.workspaces.mu.Lock()
		rows, err := d.workspaces.records()
		d.workspaces.mu.Unlock()
		present := make(map[string]bool)
		if err == nil {
			stats.Workspaces = &proto.StorageCategory{}
			for _, row := range rows {
				if row.Owner != d.workspaceOwner(id) {
					continue
				}
				for _, jobID := range row.JobIDs {
					workspaceJobs[jobID] = row
				}
				if row.CacheLeaseID != "" {
					workspaceCacheLeases[row.CacheLeaseID] = row
				}
				var usage proto.WorkspaceStorage
				usage, err = workspaceStorageBytes(r.Context(), filepath.Join(d.workspaces.dir, row.ID), row)
				if os.IsNotExist(err) {
					// Removal may have renamed the directory after enumeration.
					err = nil
					continue
				}
				if err != nil {
					break
				}
				present[row.ID] = true
				stats.Workspaces.Items++
				stats.Workspaces.Bytes += usage.Bytes
				if stats.Details != nil {
					stats.Details.Workspaces = append(stats.Details.Workspaces, usage)
				}
			}
		}
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		if err := d.workspaces.addUploadStorage(r.Context(), d.workspaceOwner(id), &stats, present); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	if d.namedCaches != nil {
		entries, err := d.namedCaches.Inventory(r.Context())
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		stats.NamedCaches = namedCacheStats(entries, id.Owner(), d.cfg.InsecureNoAuth)
		if stats.Details != nil {
			for _, entry := range entries {
				if !d.cfg.InsecureNoAuth && entry.Key.Owner != id.Owner() {
					continue
				}
				detail := proto.NamedCacheStorage{Name: entry.Key.Name, ProjectID: entry.Key.Project, JobID: entry.LeaseID, Bytes: entry.Bytes, BytesUnknown: entry.BytesUnknown}
				if len(entry.Holders) != 0 {
					detail.JobIDs = append([]string{}, entry.Holders...)
					workspace, found := workspaceJobs[entry.Holders[0]]
					if found && workspace.Owner == entry.Key.Owner {
						same := true
						for _, holder := range entry.Holders {
							same = same && workspaceJobs[holder].ID == workspace.ID
						}
						if same {
							detail.WorkspaceID = workspace.ID
						}
					}
				}
				if workspace, ok := workspaceCacheLeases[entry.LeaseID]; ok && workspace.Owner == entry.Key.Owner {
					detail.JobID, detail.WorkspaceID, detail.JobIDs = "", workspace.ID, workspace.JobIDs
				}
				stats.Details.NamedCaches = append(stats.Details.NamedCaches, detail)
			}
			sort.Slice(stats.Details.NamedCaches, func(i, j int) bool {
				a, b := stats.Details.NamedCaches[i], stats.Details.NamedCaches[j]
				if a.Name != b.Name {
					return a.Name < b.Name
				}
				return a.ProjectID < b.ProjectID
			})
		}
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
	jobDetails := make(map[string]proto.JobStorage)
	for _, job := range d.jobs {
		if d.ownsJob(id, job) {
			roots = append(roots, job.Dir)
			if stats.Details != nil {
				job.mu.Lock()
				jobDetails[job.Dir] = proto.JobStorage{ID: job.ID, WorkspaceID: job.Spec.WorkspaceID}
				job.mu.Unlock()
			}
		}
	}
	d.mu.Unlock()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		jobID, ok := gcTombstoneJobID(entry.Name())
		if ok && d.ownsCollectedJob(id, jobID) {
			root := filepath.Join(d.jobsDir(), entry.Name())
			roots = append(roots, root)
			jobDetails[root] = proto.JobStorage{ID: jobID, CleanupPending: true}
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
		if stats.Details != nil {
			detail := jobDetails[root]
			detail.Bytes = bytes
			stats.Details.Jobs = append(stats.Details.Jobs, detail)
		}
	}
	if stats.Details != nil {
		sort.Slice(stats.Details.Jobs, func(i, j int) bool { return stats.Details.Jobs[i].ID > stats.Details.Jobs[j].ID })
	}
	if d.cfg.ChangeStorage != nil {
		changes, err := d.cfg.ChangeStorage(r.Context())
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			httpError(w, http.StatusInternalServerError, "cannot inspect fetched-change storage")
			return
		}
		stats.Changes = &changes
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
