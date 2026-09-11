package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/lydakis/errand/internal/namedcache"
	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
)

// Every explicitly declared cache gets the same directory semantics. Neither
// the name, path, command, nor repository contents selects special behavior.
func (j *Job) prepareNamedCacheTrees(ctx context.Context, d *Daemon) error {
	if len(j.Spec.Selection.Caches) == 0 {
		return nil
	}
	if d.cfg.NamedCacheDisabled {
		return fmt.Errorf("named caches are disabled")
	}
	var record *workspaceRecord
	if j.Spec.WorkspaceID != "" {
		unlock, err := d.workspaces.lockWorkspaceContext(ctx, j.workspaceLeaseID)
		if err != nil {
			return err
		}
		defer unlock()
		d.workspaces.mu.Lock()
		r, err := d.workspaces.read(j.workspaceLeaseID)
		d.workspaces.mu.Unlock()
		if err != nil {
			return err
		}
		if !r.holds(j.ID) {
			return fmt.Errorf("job no longer holds workspace")
		}
		if r.TreeBaselines == nil {
			r.TreeBaselines = make(map[string]string)
		}
		record = &r
	}
	j.treeBaselines = make(map[string]string)
	for _, binding := range j.Spec.Selection.Caches {
		if err := pathpolicy.ValidateCacheCasing(j.workspacePath(), []proto.CacheBinding{binding}); err != nil {
			return err
		}
		key := namedcache.Key{Owner: d.cacheOwner(j), Project: j.Spec.CacheProjectID, Name: binding.Name}
		if _, err := d.namedCaches.AcquireTree(ctx, key, j.ID); err != nil {
			return fmt.Errorf("cache %q: %w", binding.Name, err)
		}
		base, exists := "", false
		if record != nil {
			base, exists = record.TreeBaselines[binding.Name]
		}
		// Several members can be admitted before any finishes preparation.
		// Repair until one can start using the directories, then preserve its
		// live changes for all siblings, including deliberate deletions.
		if exists && base != "absent" && record.CacheRestorePending {
			if _, err := os.Lstat(filepath.Join(j.workspacePath(), binding.Path)); os.IsNotExist(err) {
				exists = false
			}
		}
		saveBase := func(value string) error {
			if record == nil {
				return nil
			}
			record.TreeBaselines[binding.Name] = value
			if !slices.Contains(record.TreeCaches, binding.Name) {
				record.TreeCaches = append(record.TreeCaches, binding.Name)
			}
			d.workspaces.mu.Lock()
			defer d.workspaces.mu.Unlock()
			return d.workspaces.write(*record)
		}
		if !exists {
			var err error
			// Preserve live directories from older workspace receipts. New
			// restores checkpoint their mode before exposing the directory.
			info, statErr := os.Lstat(filepath.Join(j.workspacePath(), binding.Path))
			if record != nil && statErr == nil && info.IsDir() {
				base, err = namedcache.TreeFingerprint(ctx, j.workspacePath(), binding.Path)
				if err == nil {
					err = saveBase(base)
				}
			} else {
				base, err = d.namedCaches.RestoreTree(ctx, key, j.ID, j.workspacePath(), binding.Path, saveBase)
			}
			if err != nil {
				return fmt.Errorf("restoring cache %q: %w", binding.Name, err)
			}
			j.event("named-cache-restored", binding.Name+": "+binding.Path)
		}
		j.treeBaselines[binding.Name] = base
	}
	if record != nil && record.CacheRestorePending {
		// The workspace gate spans preparation and this durable transition.
		// No member may launch until later admissions know the trees are live.
		record.CacheRestorePending = false
		d.workspaces.mu.Lock()
		defer d.workspaces.mu.Unlock()
		return d.workspaces.write(*record)
	}
	return nil
}

func (d *Daemon) publishNamedCacheTrees(j *Job, baselines map[string]string) error {
	if len(baselines) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), changeCollectionTimeout)
	defer cancel()
	for _, binding := range j.Spec.Selection.Caches {
		base, ok := baselines[binding.Name]
		if !ok {
			continue
		}
		key := namedcache.Key{Owner: d.cacheOwner(j), Project: j.Spec.CacheProjectID, Name: binding.Name}
		next, err := d.namedCaches.PublishTree(ctx, key, j.ID, j.workspacePath(), binding.Path, base, j.Spec.WorkspaceID == "")
		if next != "" {
			if j.Spec.WorkspaceID == "" {
				// A completed tree may have moved out of this workspace. A retry
				// after another binding fails must not publish its absence.
				delete(baselines, binding.Name)
			} else {
				baselines[binding.Name] = next
			}
			if next != base {
				j.event("named-cache-published", binding.Name+": "+binding.Path)
			}
		}
		if err != nil {
			return fmt.Errorf("publishing cache %q: %w", binding.Name, err)
		}
	}
	return nil
}
