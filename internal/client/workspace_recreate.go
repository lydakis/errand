package client

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

// WorkspaceRecreation is what recreating a workspace deletes and keeps,
// established before anything is removed. Recreating removes the runner
// workspace and creates one with the same name, project, artifacts and caches
// from the checkout that created it.
type WorkspaceRecreation struct {
	PeerURL   string
	Workspace proto.Workspace // without its creation manifest
	// Root is the checkout the new workspace is created from: the one that
	// created this workspace, or empty when this machine holds no transfer
	// state for it and the caller must choose one.
	Root       string
	NoSnapshot bool
	// EarlierState is set when this machine's transfer state for the workspace
	// was recorded by an earlier errand. Its jobs cannot be applied with this
	// version, and recreating discards that state.
	EarlierState bool
	// Jobs are the workspace's finished jobs whose receipts the runner still
	// retains, newest first. Removing the workspace keeps them.
	Jobs []proto.JobListEntry
	// WorkingBytes is the runner's measurement of the working tree, or -1.
	WorkingBytes int64
}

// PrepareWorkspaceRecreation checks that the workspace can be recreated and
// finishes any interrupted application into its checkout. It removes nothing.
func PrepareWorkspaceRecreation(peerURL, name string) (WorkspaceRecreation, error) {
	r := WorkspaceRecreation{PeerURL: peerURL, WorkingBytes: -1}
	ws, err := GetWorkspace(peerURL, name)
	if err != nil {
		return r, err
	}
	r.NoSnapshot = len(ws.Manifest.Entries) == 0
	ws.Manifest = proto.Manifest{}
	r.Workspace = ws
	if len(ws.JobIDs) > 0 {
		return r, fmt.Errorf("workspace %s is in use by %s; recreate it after they finish", ws.Name, strings.Join(ws.JobIDs, ", "))
	}
	dir, err := workspaceTransferDir(peerURL, ws.ID)
	if err != nil {
		return r, err
	}
	origin, err := readWorkspaceOrigin(dir)
	var earlier *EarlierTransferStateError
	switch {
	case err == nil:
	case errors.As(err, &earlier):
		r.EarlierState = true
	case os.IsNotExist(err):
		origin = workspaceOrigin{}
	default:
		return r, fmt.Errorf("reading this machine's transfer state for workspace %s: %w", ws.Name, err)
	}
	if origin.Root != "" {
		moved, err := origin.rootMoved()
		if err != nil {
			return r, err
		}
		if moved {
			return r, fmt.Errorf("the checkout that created workspace %s is no longer at %s", ws.Name, origin.Root)
		}
		if err := withWorkspaceChangeLock(origin.Root, func() error { return recoverWorkspaceApplications(origin.Root) }); err != nil {
			return r, fmt.Errorf("finishing an interrupted apply into %s: %w", origin.Root, err)
		}
		r.Root = origin.Root
	}
	jobs, err := listWorkspaceJobs(peerURL, false, ws.ID)
	if err != nil {
		return r, err
	}
	for _, job := range jobs {
		if job.FinishedAt != nil {
			r.Jobs = append(r.Jobs, job)
		}
	}
	slices.SortFunc(r.Jobs, func(a, b proto.JobListEntry) int { return b.AdmittedAt.Compare(a.AdmittedAt) })
	// The size only informs the preview; a runner that cannot measure it
	// still recreates.
	if stats, err := StorageStatsDetailed(peerURL); err == nil {
		for _, w := range stats.Details.Workspaces {
			if w.ID == ws.ID {
				r.WorkingBytes = w.WorkingBytes
			}
		}
	}
	return r, nil
}

// Recreate removes the workspace and creates it again. opts supplies only
// IncludeAll and Stderr; everything else comes from the recreation. The new
// snapshot is selected before removal, so a local refusal changes nothing.
func (r WorkspaceRecreation) Recreate(opts RunOptions) (proto.Workspace, error) {
	if r.Root == "" {
		return proto.Workspace{}, fmt.Errorf("recreating workspace %s requires a checkout", r.Workspace.Name)
	}
	opts = RunOptions{
		PeerURL: r.PeerURL, Root: r.Root, Project: r.Workspace.Project, NoSnapshot: r.NoSnapshot,
		Artifacts: r.Workspace.Selection.Artifacts, Caches: r.Workspace.Selection.Caches,
		IncludeAll: opts.IncludeAll && !r.NoSnapshot, Stderr: opts.Stderr,
	}
	prepared, err := prepareWorkspace(opts, r.Workspace.Name)
	if err != nil {
		return proto.Workspace{}, err
	}
	if err := RemoveWorkspace(r.PeerURL, r.Workspace.ID); err != nil {
		return proto.Workspace{}, err
	}
	if r.EarlierState {
		// Recover again under the checkout lock, so no apply can be left
		// behind in the state being discarded.
		err := withWorkspaceChangeLock(r.Root, func() error {
			if err := recoverWorkspaceApplications(r.Root); err != nil {
				return err
			}
			return discardWorkspaceOrigin(r.PeerURL, r.Workspace.ID)
		})
		if err != nil {
			return proto.Workspace{}, &RecreateIncompleteError{r, fmt.Errorf("discarding earlier transfer state: %w", err)}
		}
	}
	w, err := prepared.create(opts)
	if err != nil {
		return w, &RecreateIncompleteError{r, err}
	}
	return w, nil
}

// RecreateIncompleteError reports a recreation that removed the workspace but
// did not create it again. Creating it is all that remains.
type RecreateIncompleteError struct {
	Recreation WorkspaceRecreation
	Err        error
}

func (e *RecreateIncompleteError) Error() string {
	return fmt.Sprintf("workspace %s was removed, but creating it again failed: %v", e.Recreation.Workspace.Name, e.Err)
}

func (e *RecreateIncompleteError) Unwrap() error { return e.Err }
