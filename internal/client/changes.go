package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
)

type ChangeFetchOptions struct {
	PeerURL   string
	JobID     string
	Apply     bool
	Path      string
	CallerDir string
}

func initializeChangeState(ctx context.Context, opts *RunOptions, jobID, manifestRoot string) error {
	clientID, err := localChangeClientID()
	if err != nil {
		return fmt.Errorf("loading local change client identity: %w", err)
	}
	opts.changeClientID = clientID
	state := localChangeState{
		JobID: jobID, PeerURL: strings.TrimSuffix(opts.PeerURL, "/"), Root: opts.Root,
		ManifestRoot: manifestRoot,
	}
	if opts.Root == "" {
		return fmt.Errorf("workspace root is required")
	}
	return withWorkspaceChangeLockContext(ctx, opts.Root, func() error {
		pending, err := changeops.WorkspaceHasApplyTransactions(opts.Root)
		if err != nil {
			return fmt.Errorf("checking prior change application: %w", err)
		}
		if pending {
			return fmt.Errorf("workspace has an interrupted change application; recover it explicitly with errand fetch --apply")
		}
		rootID, info, err := fsidentity.Lstat(opts.Root)
		if err != nil {
			return fmt.Errorf("recording workspace identity: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("recording workspace identity: root is not a directory")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		state.RootID = rootID
		return saveLocalChangeState(state)
	})
}

// FetchChanges downloads and verifies a terminal bundle. With Apply false it
// only stages the bundle. With Apply true it requires the submission machine's
// workspace identity record and applies every still-unapplied changed path.
func FetchChanges(opts ChangeFetchOptions) (string, error) {
	details, err := GetJobDetails(opts.PeerURL, opts.JobID)
	if err != nil {
		return "", err
	}
	status := details.JobStatus
	if status.Result == nil {
		return "", fmt.Errorf("job is not terminal")
	}
	if err := markLocalChangeTerminal(opts.PeerURL, opts.JobID); err != nil {
		return "", fmt.Errorf("recording terminal change state: %w", err)
	}
	if status.Result.Changes == nil {
		if !status.Result.ChangesOK {
			if status.Result.TransactionError != "" {
				return "", fmt.Errorf("job's workspace changes were not retained: %s", status.Result.TransactionError)
			}
			return "", fmt.Errorf("job's workspace changes were not retained")
		}
		return "", fmt.Errorf("job produced no workspace changes")
	}
	var staged string
	var bundle proto.ChangeBundle
	if opts.Apply {
		key := localChangeKey(opts.PeerURL, opts.JobID)
		unlock, lockErr := acquireLocalChangeLock(localChangeTransferLockName(key))
		if lockErr != nil {
			return "", lockErr
		}
		defer unlock()
		staged, bundle, err = downloadChangeBundleLocked(
			opts.PeerURL, opts.JobID, key, *status.Result.Changes,
		)
	} else {
		staged, bundle, err = downloadChangeBundle(opts.PeerURL, opts.JobID, *status.Result.Changes)
	}
	if err != nil {
		return "", err
	}
	selected, err := selectChangePath(bundle, opts.Path)
	if err != nil {
		return staged, err
	}
	if opts.Apply {
		if err := validateApplySelection(selected, opts.Path); err != nil {
			return staged, err
		}
		if _, err := applyChangeBundle(opts.PeerURL, opts.JobID, opts.CallerDir, staged, bundle, selected); err != nil {
			return staged, err
		}
	}
	if opts.Path != "" {
		return fetchedChangePath(staged, bundle, selected, opts.Path), nil
	}
	return staged, nil
}

func fetchedChangePath(staged string, bundle proto.ChangeBundle, selected map[string]bool, requested string) string {
	if !manifestContainsPath(bundle.RemoteManifest, requested) {
		return filepath.Join(staged, "bundle.json")
	}
	allDeleted := len(selected) > 0
	for _, changed := range bundle.Paths {
		if selected[changed] && manifestContainsPath(bundle.RemoteManifest, changed) {
			allDeleted = false
			break
		}
	}
	if allDeleted {
		return filepath.Join(staged, "bundle.json")
	}
	return filepath.Join(staged, "remote", filepath.FromSlash(requested))
}

func selectChangePath(bundle proto.ChangeBundle, changePath string) (map[string]bool, error) {
	if changePath == "" {
		return nil, nil
	}
	selected := map[string]bool{}
	for _, changed := range bundle.Paths {
		if changePath == changed || strings.HasPrefix(changed, strings.TrimSuffix(changePath, "/")+"/") {
			selected[changed] = true
		}
	}
	if len(selected) == 0 &&
		(manifestContainsPath(bundle.RemoteManifest, changePath) || manifestContainsPath(bundle.BaseManifest, changePath)) {
		for _, changed := range bundle.Paths {
			if strings.HasPrefix(changePath, changed+"/") {
				selected[changed] = true
				break
			}
		}
	}
	if len(selected) != 0 {
		return selected, nil
	}
	return nil, fmt.Errorf("path %q was not changed by this job", changePath)
}

func manifestContainsPath(manifest proto.Manifest, changePath string) bool {
	index := sort.Search(len(manifest.Entries), func(i int) bool {
		return manifest.Entries[i].Path >= changePath
	})
	return index < len(manifest.Entries) && manifest.Entries[index].Path == changePath
}

func validateApplySelection(selected map[string]bool, requested string) error {
	if requested == "" {
		return nil
	}
	for root := range selected {
		if strings.HasPrefix(requested, root+"/") {
			return fmt.Errorf("path %q is inside retained change root %q; apply the root instead", requested, root)
		}
	}
	return nil
}

func applyChangeBundle(peerURL, jobID, callerDir, staged string, bundle proto.ChangeBundle, selected map[string]bool) ([]string, error) {
	state, err := loadLocalChangeState(peerURL, jobID)
	if err != nil {
		return nil, fmt.Errorf("applying changes: %w", err)
	}
	if bundle.BaselineRoot != state.ManifestRoot {
		return nil, fmt.Errorf("applying changes: retained changes do not match the submitted workspace")
	}
	if err := validateApplyCallerWorkspace(state.Root, callerDir); err != nil {
		return nil, fmt.Errorf("applying changes: %w", err)
	}
	var applied []string
	err = withWorkspaceChangeLock(state.Root, func() error {
		state, err = loadLocalChangeState(peerURL, jobID)
		if err != nil {
			return err
		}
		if bundle.BaselineRoot != state.ManifestRoot {
			return fmt.Errorf("retained changes do not match the submitted workspace")
		}
		if err := validateApplyCallerWorkspace(state.Root, callerDir); err != nil {
			return err
		}
		if err := recoverWorkspaceApplications(state.Root); err != nil {
			return err
		}
		state, err = loadLocalChangeState(peerURL, jobID)
		if err != nil {
			return err
		}
		if bundle.BaselineRoot != state.ManifestRoot {
			return fmt.Errorf("retained changes do not match the submitted workspace")
		}
		if err := validateLocalWorkspaceIdentity(state); err != nil {
			return err
		}
		if state.Applied == nil {
			state.Applied = map[string]string{}
		}
		effective := map[string]bool{}
		for _, changePath := range bundle.Paths {
			if selected != nil && !selected[changePath] {
				continue
			}
			if appliedState, ok := state.Applied[changePath]; ok {
				matches, matchErr := changeops.ChangePathStateMatchesWorkspace(
					state.Root, state.RootID, changePath, appliedState,
				)
				if matchErr != nil {
					return matchErr
				}
				if !matches {
					return fmt.Errorf("change %q changed after it was applied", changePath)
				}
				continue
			}
			matches, matchErr := changeops.ChangePathMatchesWorkspace(state.Root, state.RootID, bundle, changePath)
			if matchErr != nil {
				return matchErr
			}
			if matches {
				pathState, stateErr := changeops.ChangePathState(bundle, changePath)
				if stateErr != nil {
					return stateErr
				}
				state.Applied[changePath] = pathState
				applied = append(applied, changePath)
				continue
			}
			effective[changePath] = true
		}
		if len(effective) == 0 {
			return saveLocalChangeState(state)
		}
		transaction := changeops.NewApplyTransaction()
		state.Pending = transaction
		if err := saveLocalChangeState(state); err != nil {
			return err
		}
		owner := localChangeKey(peerURL, jobID)
		result, applyErr := changeops.ApplyToWorkspace(
			staged, state.Root, bundle, effective, owner, transaction,
			state.RootID,
		)
		if applyErr != nil {
			if _, recoverErr := recoverOneApplication(&state, owner); recoverErr != nil {
				return errors.Join(applyErr, recoverErr)
			}
			return applyErr
		}
		for _, changePath := range result.Applied {
			state.Applied[changePath] = result.States[changePath]
			applied = append(applied, changePath)
		}
		if err := saveLocalChangeState(state); err != nil {
			return err
		}
		if err := changeops.CommitApplyToWorkspace(state.Root, result.Transaction, state.RootID); err != nil {
			return err
		}
		state.Pending = ""
		return saveLocalChangeState(state)
	})
	sort.Strings(applied)
	if err != nil {
		return nil, fmt.Errorf("applying changes: %w", err)
	}
	return applied, nil
}

func validateApplyCallerWorkspace(recordedRoot, callerDir string) error {
	if callerDir == "" {
		return fmt.Errorf("--apply must be run from the workspace that submitted the job")
	}
	recorded, err := filepath.Abs(recordedRoot)
	if err != nil {
		return err
	}
	recorded, err = filepath.EvalSymlinks(recorded)
	if err != nil {
		return err
	}
	caller, err := filepath.Abs(callerDir)
	if err != nil {
		return err
	}
	caller, err = filepath.EvalSymlinks(caller)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(recorded, caller)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("--apply must be run from within the workspace at %q", recordedRoot)
	}
	return nil
}
