package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/lydakis/errand/internal/namedcache"
	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
)

func (d *Daemon) cacheOwner(j *Job) string {
	if d.cfg.InsecureNoAuth {
		return "insecure-test"
	}
	return admissionOwner(j.Admission)
}

// Bindings are symlinks installed only after snapshot/base capture. Parents
// must be real directories; an existing binding destination is never replaced.
func (j *Job) bindNamedCaches(d *Daemon) error {
	if len(j.Spec.Selection.Caches) == 0 {
		return nil
	}
	if d.cfg.NamedCacheDisabled {
		return fmt.Errorf("named caches are disabled")
	}
	workspace := filepath.Join(j.Dir, "workspace")
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, cache := range j.Spec.Selection.Caches {
		// Earlier bindings may have created a shared parent. Check each binding
		// against those directories before acquiring its lease or starting work.
		if err := pathpolicy.ValidateCacheCasing(workspace, []proto.CacheBinding{cache}); err != nil {
			return err
		}
		parent := path.Dir(cache.Path)
		current := ""
		if parent != "." {
			for _, part := range strings.Split(parent, "/") {
				current = path.Join(current, part)
				if err := root.Mkdir(current, 0700); err != nil && !os.IsExist(err) {
					return err
				}
				info, err := root.Lstat(current)
				if err != nil {
					return err
				}
				if !info.IsDir() {
					return fmt.Errorf("cache parent %q must be a directory", current)
				}
			}
		}
		if _, err := root.Lstat(cache.Path); !os.IsNotExist(err) {
			return fmt.Errorf("cache destination %q already exists or is inaccessible", cache.Path)
		}
		key := namedcache.Key{Owner: d.cacheOwner(j), Project: j.Spec.CacheProjectID, Name: cache.Name}
		data, err := d.namedCaches.Acquire(context.Background(), key, j.ID)
		if err != nil {
			return fmt.Errorf("cache %q: %w", cache.Name, err)
		}
		if err := root.Symlink(data, cache.Path); err != nil {
			return err
		}
	}
	return nil
}

// Inventory finds leases even when acquisition failed after publishing its
// record. A failed release is discarded only after confirmed process cleanup.
func (d *Daemon) settleNamedCaches(j *Job) error {
	if d.namedCaches == nil || len(j.Spec.Selection.Caches) == 0 {
		return nil
	}
	entries, err := d.namedCaches.Inventory(context.Background())
	if err != nil {
		return err
	}
	var joined error
	for _, entry := range entries {
		if entry.LeaseID != j.ID || entry.Key.Owner != d.cacheOwner(j) {
			continue
		}
		if err := d.namedCaches.Release(context.Background(), entry.Key, j.ID); err != nil {
			// A post-rename sync failure may already have cleared the lease. Read
			// back before choosing the destructive fallback or reporting failure.
			current, readErr := d.namedCaches.Inventory(context.Background())
			if readErr != nil {
				joined = errors.Join(joined, err, readErr)
				continue
			}
			for _, state := range current {
				if state.Key == entry.Key && state.LeaseID == j.ID {
					if discardErr := d.namedCaches.Discard(context.Background(), entry.Key, j.ID); discardErr != nil {
						joined = errors.Join(joined, err, discardErr)
					}
					j.event("named-cache-discarded", entry.Key.Name)
				}
			}
		}
	}
	return joined
}

func (d *Daemon) recoverNamedCaches() error {
	entries, err := d.namedCaches.Inventory(context.Background())
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.LeaseID == "" || seen[entry.LeaseID] {
			continue
		}
		j := d.jobs[entry.LeaseID]
		if j == nil || d.cacheOwner(j) != entry.Key.Owner {
			continue
		}
		seen[j.ID] = true
		// Missing/unreadable receipt identity cannot authorize cache reuse.
		if j.Spec.CacheProjectID != entry.Key.Project || len(j.Spec.Selection.Caches) == 0 {
			continue
		}
		_, cleanupErrs := d.cleanupPersistedRuntime(j)
		if len(cleanupErrs) != 0 {
			j.event("named-cache-recovery-protected", strings.Join(cleanupErrs, "; "))
			continue
		}
		if err := d.settleNamedCaches(j); err != nil {
			j.event("named-cache-recovery-failed", err.Error())
		}
	}
	return nil
}

func namedCacheStats(entries []namedcache.Entry, owner string, all bool) *proto.NamedCacheStats {
	stats := &proto.NamedCacheStats{}
	for _, entry := range entries {
		if !all && entry.Key.Owner != owner {
			continue
		}
		stats.Items++
		stats.Bytes += entry.Bytes
		if entry.LeaseID != "" {
			stats.Protected++
		}
	}
	return stats
}

// Recovery uses current leases, never paths saved by a job that may have
// released its caches before a crash. Unreadable lease metadata fails closed.
func (d *Daemon) cleanupPersistedRuntime(j *Job) ([]int, []string) {
	var dirs []string
	if d.namedCaches != nil {
		var err error
		dirs, err = d.namedCaches.LeasePaths(context.Background(), j.ID)
		if err != nil {
			return nil, []string{"reading cache process scope: " + err.Error()}
		}
	}
	return cleanupPersistedRuntime(j, dirs...)
}
