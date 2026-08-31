package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	outputops "github.com/lydakis/errand/internal/outputs"
	"github.com/lydakis/errand/internal/proto"
)

type OutputFetchOptions struct {
	PeerURL    string
	JobID      string
	Apply      bool
	OutputPath string
	CallerDir  string
}

func initializeOutputState(ctx context.Context, opts *RunOptions, jobID string) error {
	normalized, err := outputops.NormalizeSpecs(opts.Outputs)
	if err != nil {
		return err
	}
	opts.Outputs = normalized
	if opts.NoSnapshot {
		if len(normalized) > 0 {
			return fmt.Errorf("declared outputs require a local snapshot")
		}
		return nil
	}
	if opts.Root == "" {
		if len(normalized) == 0 {
			return nil
		}
		return fmt.Errorf("declared outputs require a local workspace root")
	}
	if len(normalized) > 0 {
		clientID, err := localOutputClientID()
		if err != nil {
			return fmt.Errorf("loading local output client identity: %w", err)
		}
		opts.outputClientID = clientID
	}
	return withWorkspaceOutputLockContext(ctx, opts.Root, func() error {
		if err := recoverWorkspaceApplicationsContext(ctx, opts.Root); err != nil {
			return fmt.Errorf("recovering prior output application: %w", err)
		}
		if len(normalized) == 0 {
			return nil
		}
		baselines, rootID, err := outputops.CaptureWorkspaceBaselinesContext(
			ctx, opts.Root, normalized, proto.DefaultLimits().MaxOutputBytes, outputops.MaxOutputEntries,
		)
		if err != nil {
			return fmt.Errorf("capturing output baselines: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return saveLocalOutputState(localOutputState{
			Version: localOutputStateVersion, JobID: jobID, PeerURL: strings.TrimSuffix(opts.PeerURL, "/"),
			Root: opts.Root, RootID: rootID, Outputs: normalized, Baselines: baselines,
		})
	})
}

// FetchOutputs downloads and verifies a terminal bundle. With Apply false it
// only stages the bundle. With Apply true it requires the submission machine's
// baseline record and applies every still-unapplied declared path.
func FetchOutputs(opts OutputFetchOptions) (string, error) {
	status, err := getStatus(opts.PeerURL, opts.JobID)
	if err != nil {
		return "", err
	}
	if status.Result == nil {
		return "", fmt.Errorf("job is not terminal")
	}
	if err := markLocalOutputTerminal(opts.PeerURL, opts.JobID); err != nil {
		return "", fmt.Errorf("recording terminal output state: %w", err)
	}
	if status.Result.Outputs == nil {
		return "", fmt.Errorf("job has no collected outputs")
	}
	staged, bundle, err := downloadOutputBundle(opts.PeerURL, opts.JobID, *status.Result.Outputs)
	if err != nil {
		return "", err
	}
	selected, err := selectOutputPath(bundle, opts.OutputPath)
	if err != nil {
		return staged, err
	}
	if opts.Apply {
		if _, err := applyOutputBundle(opts.PeerURL, opts.JobID, opts.CallerDir, staged, bundle, selected); err != nil {
			return staged, err
		}
	}
	if opts.OutputPath != "" {
		return filepath.Join(staged, "workspace", filepath.FromSlash(opts.OutputPath)), nil
	}
	return staged, nil
}

func selectOutputPath(bundle proto.OutputBundle, outputPath string) (map[string]bool, error) {
	if outputPath == "" {
		return nil, nil
	}
	for _, declared := range bundle.Paths {
		if outputPath == declared {
			return map[string]bool{outputPath: true}, nil
		}
	}
	return nil, fmt.Errorf("output path %q was not declared for this job", outputPath)
}

func materializeTerminalOutputs(opts RunOptions, jobID string, status proto.JobStatus, applyAuto, applyAll bool) (string, []string, error) {
	if status.Result == nil {
		return "", nil, nil
	}
	if err := markLocalOutputTerminal(opts.PeerURL, jobID); err != nil {
		return "", nil, fmt.Errorf("recording terminal output state: %w", err)
	}
	if status.Result.Outputs == nil {
		return "", nil, nil
	}
	staged, bundle, err := downloadOutputBundle(opts.PeerURL, jobID, *status.Result.Outputs)
	if err != nil {
		return "", nil, err
	}
	if !applyAuto && !applyAll {
		return staged, nil, nil
	}
	state, err := loadLocalOutputState(opts.PeerURL, jobID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !applyAll {
			return staged, nil, nil
		}
		return staged, nil, fmt.Errorf("applying outputs: %w", err)
	}
	selected := map[string]bool{}
	for _, spec := range state.Outputs {
		if applyAll || (applyAuto && spec.Apply == proto.OutputApplyAuto) {
			selected[spec.Path] = true
		}
	}
	if len(selected) == 0 {
		return staged, nil, nil
	}
	applied, err := applyOutputBundle(opts.PeerURL, jobID, opts.Root, staged, bundle, selected)
	if err != nil {
		return staged, nil, err
	}
	return staged, applied, nil
}

func applyOutputBundle(peerURL, jobID, callerDir, staged string, bundle proto.OutputBundle, selected map[string]bool) ([]string, error) {
	state, err := loadLocalOutputState(peerURL, jobID)
	if err != nil {
		return nil, fmt.Errorf("applying outputs: %w", err)
	}
	if err := validateApplyCallerWorkspace(state.Root, callerDir); err != nil {
		return nil, fmt.Errorf("applying outputs: %w", err)
	}
	var applied []string
	err = withWorkspaceOutputLock(state.Root, func() error {
		state, err = loadLocalOutputState(peerURL, jobID)
		if err != nil {
			return err
		}
		if err := validateApplyCallerWorkspace(state.Root, callerDir); err != nil {
			return err
		}
		if err := recoverWorkspaceApplications(state.Root); err != nil {
			return err
		}
		state, err = loadLocalOutputState(peerURL, jobID)
		if err != nil {
			return err
		}
		if err := validateLocalWorkspaceIdentity(state); err != nil {
			return err
		}
		if state.Applied == nil {
			state.Applied = map[string]string{}
		}
		bundleRoot := bundle.Manifest.RootHash()
		effective := map[string]bool{}
		for _, outputPath := range bundle.Paths {
			if selected != nil && !selected[outputPath] {
				continue
			}
			matches, matchErr := outputops.OutputPathMatchesWorkspace(state.Root, state.RootID, bundle, outputPath)
			if matchErr != nil {
				return matchErr
			}
			if state.Applied[outputPath] == bundleRoot {
				if !matches {
					return fmt.Errorf("output %q changed after it was applied", outputPath)
				}
				continue
			}
			if matches {
				state.Applied[outputPath] = bundleRoot
				applied = append(applied, outputPath)
				continue
			}
			effective[outputPath] = true
		}
		if len(effective) == 0 {
			return saveLocalOutputState(state)
		}
		transaction := outputops.NewApplyTransaction()
		state.Pending = transaction
		if err := saveLocalOutputState(state); err != nil {
			return err
		}
		owner := localOutputKey(peerURL, jobID)
		result, applyErr := outputops.ApplyToWorkspace(
			filepath.Join(staged, "workspace"), state.Root, bundle, state.Baselines, effective, owner, transaction,
			state.RootID,
		)
		if applyErr != nil {
			if _, recoverErr := recoverOneApplication(&state, owner); recoverErr != nil {
				return errors.Join(applyErr, recoverErr)
			}
			return applyErr
		}
		for _, outputPath := range result.Applied {
			state.Applied[outputPath] = result.BundleRoot
			applied = append(applied, outputPath)
		}
		if err := saveLocalOutputState(state); err != nil {
			return err
		}
		if err := outputops.CommitApplyToWorkspace(state.Root, result.Transaction, state.RootID); err != nil {
			return err
		}
		state.Pending = ""
		return saveLocalOutputState(state)
	})
	sort.Strings(applied)
	if err != nil {
		return nil, fmt.Errorf("applying outputs: %w", err)
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
