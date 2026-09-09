package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/proto"
)

const workspaceLeaseFile = "workspace-lease.json"

type workspaceLeaseRef struct {
	WorkspaceID string `json:"workspace_id"`
}

func (d *Daemon) acquireWorkspace(ctx context.Context, j *Job) error {
	s := d.workspaces
	var record workspaceRecord
	err := func() error {
		unlock := s.lockWorkspace(j.Spec.WorkspaceID)
		defer unlock()
		s.mu.Lock()
		r, err := s.lookup(d.cacheOwner(j), j.Spec.WorkspaceID)
		s.mu.Unlock()
		if err != nil {
			return fmt.Errorf("workspace does not exist: %w", err)
		}
		if err := d.recoverWorkspacePushes(r); err != nil {
			return err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.holds(j.ID) {
			return fmt.Errorf("job already holds workspace")
		}
		if r.Manifest.RootHash() != j.Spec.ManifestRoot || r.CacheProjectID != j.Spec.CacheProjectID {
			return fmt.Errorf("workspace creation snapshot does not match request")
		}
		policy := j.Spec.Selection
		policy.Artifacts = r.Selection.Artifacts
		if !sameWorkspacePolicy(policy, r.Selection) {
			return fmt.Errorf("workspace selection policy does not match creation")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		data := filepath.Join(s.dir, r.ID, "data")
		if err := workspaceDataIdentity(data, r.Identity); err != nil {
			return err
		}
		// Persist the reverse reference first. Even a failed lease publication
		// can then be reconciled without decoding the spec or scanning siblings.
		j.workspaceLeaseID = r.ID
		if err := replaceJSONDurable(filepath.Join(j.Dir, workspaceLeaseFile), workspaceLeaseRef{r.ID}); err != nil {
			return err
		}
		if len(r.JobIDs) == 0 {
			r.CacheLeaseID = j.ID
		}
		r.JobIDs = append(r.JobIDs, j.ID)
		if err := s.write(r); err != nil {
			return err
		}
		j.workspaceRoot, j.baseline, record = data, r.Manifest, r
		return nil
	}()
	if err != nil {
		return err
	}
	// The durable lease protects the tree; copying must not hold the global
	// metadata mutex or delay status/cancellation of unrelated jobs.
	return changeops.CaptureWorkspaceBaseContext(ctx, filepath.Join(s.dir, record.ID, "change-base"), j.Dir, record.Manifest)
}

func sameWorkspacePolicy(a, b proto.SelectionPolicy) bool {
	return (proto.Spec{Selection: a}).Digest() == (proto.Spec{Selection: b}).Digest()
}

func (d *Daemon) returnWorkspace(j *Job) error {
	if j.workspaceLeaseErr != nil {
		return j.workspaceLeaseErr
	}
	s := d.workspaces
	if s == nil || j.workspaceLeaseID == "" {
		return removeOwnedTree(filepath.Join(j.Dir, "workspace"))
	}
	unlock := s.lockWorkspace(j.workspaceLeaseID)
	defer unlock()
	s.mu.Lock()
	r, err := s.read(j.workspaceLeaseID)
	s.mu.Unlock()
	if os.IsNotExist(err) {
		// A removed workspace is safe; a missing metadata file inside an
		// existing workspace is damaged lease state and must stay protected.
		if _, statErr := s.root.Lstat(j.workspaceLeaseID); os.IsNotExist(statErr) {
			return nil
		}
	}
	if err != nil {
		return err
	}
	if !r.holds(j.ID) {
		// Never clean a newer job's tree.
		return nil
	}
	data := filepath.Join(s.dir, r.ID, "data")
	if err := workspaceDataIdentity(data, r.Identity); err != nil {
		return err
	}
	// Shared bindings survive until every process scope has been cleaned and
	// the final member returns. Failed settlement retains the last lease.
	if len(r.JobIDs) == 1 {
		if err := settlePersistentCachePaths(j, data, r.Selection.Caches); err != nil {
			return err
		}
		if len(r.Selection.Caches) != 0 {
			if err := d.settleNamedCacheLease(j, r.Owner, r.CacheLeaseID); err != nil {
				return err
			}
		}
		if err := syncDirectory(data); err != nil {
			return err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	r, err = s.read(r.ID)
	if err != nil {
		return err
	}
	if !r.holds(j.ID) {
		return nil
	}
	r.JobIDs = slices.DeleteFunc(r.JobIDs, func(id string) bool { return id == j.ID })
	if len(r.JobIDs) == 0 {
		r.CacheLeaseID = ""
	}
	return s.write(r)
}

// A reference identifies a candidate only. The current durable lease decides
// whether this job may use that directory for process-scope recovery.
func (d *Daemon) restoreWorkspaceLease(j *Job) error {
	f, err := os.Open(filepath.Join(j.Dir, workspaceLeaseFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 1025))
	if err != nil {
		return err
	}
	var ref workspaceLeaseRef
	if len(raw) > 1024 {
		return fmt.Errorf("workspace lease reference is too large")
	}
	if err := decodeStrictJSON(raw, &ref); err != nil {
		return err
	}
	if !proto.ValidULID(ref.WorkspaceID) {
		return fmt.Errorf("invalid workspace lease reference")
	}
	j.workspaceLeaseID = ref.WorkspaceID
	r, err := d.workspaces.read(ref.WorkspaceID)
	if os.IsNotExist(err) {
		if _, statErr := d.workspaces.root.Lstat(ref.WorkspaceID); os.IsNotExist(statErr) {
			return nil
		}
	}
	if err != nil {
		return err
	}
	if !r.holds(j.ID) {
		return nil
	}
	data := filepath.Join(d.workspaces.dir, r.ID, "data")
	if err := workspaceDataIdentity(data, r.Identity); err != nil {
		return err
	}
	j.workspaceRoot = data
	return nil
}

// Cache paths are reserved bindings. Preserve replacement files in the live
// workspace instead of discarding them or permanently preventing another run.
func settlePersistentCachePaths(j *Job, workspace string, caches []proto.CacheBinding) error {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, cache := range caches {
		info, err := root.Lstat(cache.Path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if err := root.Remove(cache.Path); err != nil {
				return err
			}
			continue
		}
		// A fresh top-level directory cannot collide with a cache binding
		// named .errand-cache-recovery or a user-created recovery symlink.
		backup := ".errand-cache-recovery-" + proto.NewULID()
		if err := root.Mkdir(backup, 0700); err != nil {
			return err
		}
		backup += "/" + cache.Name
		if err := root.Rename(cache.Path, backup); err != nil {
			return err
		}
		// Sync both sides before releasing the lease. Recovery directories stay
		// available to subsequent commands and normal retained-change capture.
		for _, dir := range []string{filepath.Dir(cache.Path), filepath.Dir(backup), "."} {
			f, err := root.Open(dir)
			if err != nil {
				return err
			}
			err = f.Sync()
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
		j.event("named-cache-path-recovered", cache.Path+" moved to "+backup)
	}
	return nil
}

func (j *Job) workspacePath() string {
	if j.workspaceRoot != "" {
		return j.workspaceRoot
	}
	return filepath.Join(j.Dir, "workspace")
}

func (j *Job) cleanupWorkspace() error {
	if j.returnWorkspace != nil {
		return j.returnWorkspace()
	}
	return removeOwnedTree(filepath.Join(j.Dir, "workspace"))
}
