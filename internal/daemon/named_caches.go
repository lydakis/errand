package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lydakis/errand/internal/namedcache"
	"github.com/lydakis/errand/internal/proto"
)

func (d *Daemon) cacheOwner(j *Job) string {
	if d.cfg.InsecureNoAuth {
		return "insecure-test"
	}
	return admissionOwner(j.Admission)
}

// Materialization happens in cancellable staging. Launch only checks the
// receipt; it never serializes the queue on directory copying.
func (j *Job) bindNamedCaches(d *Daemon) error {
	if d.cfg.NamedCacheDisabled && len(j.Spec.Selection.Caches) != 0 {
		return fmt.Errorf("named caches are disabled")
	}
	for _, binding := range j.Spec.Selection.Caches {
		if _, ok := j.treeBaselines[binding.Name]; !ok {
			return fmt.Errorf("cache %q was not staged", binding.Name)
		}
	}
	return nil
}

// The durable selection receipt finds partial acquisitions too. Job cleanup is
// bounded by its own bindings, independent of other projects and holder counts.
func (j *Job) namedCacheKeys(owner string) []namedcache.Key {
	keys := make([]namedcache.Key, 0, len(j.Spec.Selection.Caches))
	for _, cache := range j.Spec.Selection.Caches {
		keys = append(keys, namedcache.Key{Owner: owner, Project: j.Spec.CacheProjectID, Name: cache.Name})
	}
	return keys
}

func (d *Daemon) settleNamedCaches(j *Job) error {
	if d.namedCaches == nil {
		return nil
	}
	var joined error
	if j.Spec.WorkspaceID == "" && j.publishTrees {
		j.cachePublicationErr = d.publishNamedCacheTrees(j, j.treeBaselines)
	}
	for i, key := range j.namedCacheKeys(d.cacheOwner(j)) {
		state, err := d.namedCaches.LookupLease(context.Background(), key, j.ID)
		if err == nil && state.Tree {
			err = d.namedCaches.DiscardRestore(context.Background(), key, j.ID, j.workspacePath(), j.Spec.Selection.Caches[i].Path)
			if err == nil {
				err = d.namedCaches.ReleaseTree(context.Background(), key, j.ID)
			}
		}
		joined = errors.Join(joined, err)
	}
	if j.Spec.WorkspaceID == "" && j.workspaceLeaseID == "" && j.treeBaselines == nil {
		joined = errors.Join(joined, d.settleNamedCacheLease(j, d.cacheOwner(j), j.ID))
	}
	return joined
}

func (d *Daemon) settleNamedCacheLease(j *Job, owner, leaseID string) error {
	if d.namedCaches == nil {
		return nil
	}
	var joined error
	for _, key := range j.namedCacheKeys(owner) {
		state, err := d.namedCaches.LookupLease(context.Background(), key, leaseID)
		if err != nil {
			joined = errors.Join(joined, err)
			continue
		}
		if !state.Directory {
			continue
		}
		if err := d.namedCaches.Release(context.Background(), key, leaseID); err != nil {
			// A failed post-rename sync may already have cleared the lease.
			// Read back this key before choosing the destructive legacy fallback.
			current, readErr := d.namedCaches.LookupLease(context.Background(), key, leaseID)
			if readErr != nil {
				joined = errors.Join(joined, err, readErr)
				continue
			}
			if current.Directory {
				if discardErr := d.namedCaches.Discard(context.Background(), key, leaseID); discardErr != nil {
					joined = errors.Join(joined, err, discardErr)
				} else {
					j.event("named-cache-discarded", key.Name)
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
		ids := append([]string{}, entry.Holders...)
		if entry.LeaseID != "" {
			ids = append(ids, entry.LeaseID)
		}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			j := d.jobs[id]
			if j == nil || d.cacheOwner(j) != entry.Key.Owner || entry.LeaseID == id && (j.Spec.WorkspaceID != "" || j.workspaceLeaseID != "") {
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
		if entry.BytesUnknown {
			stats.Unmeasured++
		}
		stats.Bytes += entry.Bytes
		if entry.Protected() {
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
		dirs, err = d.namedCaches.LeasePaths(context.Background(), j.ID, j.namedCacheKeys(d.cacheOwner(j)))
		if err != nil {
			return nil, []string{"reading cache process scope: " + err.Error()}
		}
	}
	return cleanupPersistedRuntime(j, dirs...)
}

// A failed cache settlement retains workspace membership and process evidence
// for recovery. Report that operation accurately instead of claiming removal failed.
type cacheSettlementError struct{ err error }

func (e *cacheSettlementError) Error() string { return "settling named caches: " + e.err.Error() }
func (e *cacheSettlementError) Unwrap() error { return e.err }
