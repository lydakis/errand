package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/proto"
)

type workspaceFetchAttempt struct {
	ID        string `json:"id"`
	Path      string `json:"path,omitempty"`
	Conflicts bool   `json:"conflicts,omitempty"`
	Retry     bool   `json:"retry,omitempty"`
}

// Existing job handles still identify immutable results, including after the
// live workspace is removed. Only application uses the directional checkpoint.
func applyWorkspaceJob(opts ChangeFetchOptions, details proto.JobDetails, origin workspaceOrigin, dir string) (string, error) {
	state, err := loadLocalChangeState(opts.PeerURL, opts.JobID)
	if err != nil {
		return "", err
	}
	if !sameLocalRoot(state.Root, origin.Root) || state.RootID != origin.RootID || state.ManifestRoot != origin.Initial.RootHash() {
		return "", fmt.Errorf("job and workspace origin do not match")
	}
	if err := validateApplyCallerWorkspace(origin.Root, opts.CallerDir); err != nil {
		return "", err
	}
	key := localChangeKey(opts.PeerURL, opts.JobID)
	unlock, err := acquireLocalChangeLock(localChangeTransferLockName(key))
	if err != nil {
		return "", err
	}
	defer unlock()
	var downloaded string
	b := proto.ChangeBundle{V: changeops.BundleVersion, BaselineRoot: origin.Initial.RootHash()}
	if details.Result.Changes != nil {
		downloaded, b, err = downloadChangeBundleLocked(opts.PeerURL, opts.JobID, key, *details.Result.Changes, opts.meter)
		if err != nil {
			return "", err
		}
	} else if !details.Result.ChangesOK {
		return "", fmt.Errorf("job's workspace changes were not retained")
	} else if !details.Result.Started {
		// Cancellation and queued launch failure can report ChangesOK without
		// observing the tree. They cannot establish an empty source snapshot.
		return "", fmt.Errorf("job did not run; no workspace snapshot is available to apply")
	}
	var staged string
	err = withWorkspaceChangeLock(origin.Root, func() error {
		if err := recoverWorkspaceApplications(origin.Root); err != nil {
			return err
		}
		if err := validateLocalWorkspaceIdentity(state); err != nil {
			return err
		}
		unlock, err := lockWorkspaceTransfer(dir)
		if err != nil {
			return err
		}
		defer unlock()
		session := origin.session(dir)
		indexDir := filepath.Join(dir, "fetch-jobs")
		if err := ensurePrivateLocalDirectory(indexDir); err != nil {
			return err
		}
		indexPath := filepath.Join(indexDir, opts.JobID+".json")
		var index workspaceFetchAttempt
		raw, err := os.ReadFile(indexPath)
		if err == nil {
			if err := json.Unmarshal(raw, &index); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if index.ID != "" {
			if _, err := session.Attempt(index.ID); os.IsNotExist(err) {
				if !index.Retry {
					return fmt.Errorf("this job's apply staging was collected; fetch a new job result, or use --output to inspect this retained result")
				}
				index.ID = ""
			} else if err != nil {
				return err
			}
		}
		if index.ID == "" || index.Retry || index.Path != opts.Path || index.Conflicts != opts.MaterializeConflicts {
			// Import immutable remote bodies and reconstruct the complete observed
			// source, including unchanged creation files and deletions.
			if downloaded != "" {
				if err := session.Blobs().Retain(context.Background(), filepath.Join(downloaded, "remote"), b.RemoteManifest); err != nil {
					return err
				}
			}
			previous, err := session.Checkpoint().Read()
			if err != nil {
				return err
			}
			current, err := changeops.ObservedSource(origin.Initial, b, previous.Manifest, details.Spec.Selection)
			if err != nil {
				return err
			}
			tmp, err := os.MkdirTemp(dir, ".source-")
			if err != nil {
				return err
			}
			defer changeops.RemoveTree(tmp)
			if err := session.Blobs().MaterializeBase(context.Background(), tmp, current, session.MaxSourceBytes); err != nil {
				return err
			}
			index = workspaceFetchAttempt{ID: proto.NewULID(), Path: opts.Path, Conflicts: opts.MaterializeConflicts}
			staged, _, err = session.Stage(context.Background(), index.ID, filepath.Join(tmp, "change-base"), current)
			if err != nil {
				return err
			}
			if err := replaceTransferJSON(indexPath, index); err != nil {
				return err
			}
		}
		staged = filepath.Join(session.Directory, "attempts", index.ID)
		delta, err := changeops.ReadTransferBundle(staged)
		if err != nil {
			return err
		}
		selected, err := changeops.SelectTransferPaths(delta, opts.Path)
		if err != nil {
			return err
		}
		result, applyErr := session.Apply(index.ID, selected, opts.MaterializeConflicts)
		opts.meter.paths(result.Applied, nil)
		var conflict *changeops.MergeConflictError
		if errors.As(applyErr, &conflict) {
			index.Retry = true
			if err := replaceTransferJSON(indexPath, index); err != nil {
				return err
			}
		}
		if applyErr == nil {
			markRecoveredAutomaticApply(&state, staged, opts.Path == "")
			if err := saveLocalChangeState(state); err != nil {
				return err
			}
		}
		return applyErr
	})
	return staged, err
}
