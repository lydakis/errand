package client

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	outputops "github.com/lydakis/errand/internal/outputs"
	"github.com/lydakis/errand/internal/proto"
)

type OutputGCResult struct {
	Selected   int
	Removed    int
	Protected  int
	Failed     int
	FreedBytes int64
	DryRun     bool
}

// OutputStats reports the local output state that is managed by gc outputs.
// Bytes uses the same accounting as GC so the inventory and reclaimed-space
// reports remain comparable.
func OutputStats() (proto.StorageCategory, error) {
	return outputStatsWithCollector(collectOutputGCCandidates)
}

func outputStatsWithCollector(
	collect func(string, string, map[string]*localOutputCandidate) error,
) (proto.StorageCategory, error) {
	root, err := localOutputRoot()
	if err != nil {
		return proto.StorageCategory{}, err
	}
	candidates := map[string]*localOutputCandidate{}
	if err := collect(
		filepath.Join(root, "jobs"),
		filepath.Join(root, "downloads"),
		candidates,
	); err != nil && !errors.Is(err, os.ErrNotExist) {
		return proto.StorageCategory{}, err
	}
	stats := proto.StorageCategory{Items: len(candidates)}
	for _, candidate := range candidates {
		stats.Bytes += candidate.bytes
	}
	return stats, nil
}

type localOutputCandidate struct {
	key           string
	statePath     string
	downloadPaths []string
	modified      time.Time
	bytes         int64
}

const unresolvedOutputStateProtection = proto.OutputReconciliationWindow

// OutputGC removes old local baseline records and downloaded output staging.
// Pending apply transactions are always protected; unresolved submitted jobs
// are protected for the runner's bounded reconciliation window.
func OutputGC(olderThan time.Duration, dryRun bool) (OutputGCResult, error) {
	result := OutputGCResult{DryRun: dryRun}
	if olderThan < time.Second {
		return result, fmt.Errorf("local output retention must be at least 1s")
	}
	root, err := localOutputRoot()
	if err != nil {
		return result, err
	}
	candidates := map[string]*localOutputCandidate{}
	jobs := filepath.Join(root, "jobs")
	downloads := filepath.Join(root, "downloads")
	if err := collectOutputGCCandidates(jobs, downloads, candidates); err != nil {
		return result, err
	}
	cutoff := time.Now().Add(-olderThan)
	for _, candidate := range candidates {
		if !candidate.modified.Before(cutoff) {
			continue
		}
		result.Selected++
		unlock, acquired, lockErr := tryAcquireLocalOutputLock(localOutputTransferLockName(candidate.key))
		if lockErr != nil {
			result.Failed++
			continue
		}
		if !acquired {
			result.Protected++
			continue
		}
		removed, eligible, protected, removeErr := collectLocalOutputCandidate(candidate, cutoff, dryRun)
		unlock()
		if removeErr != nil {
			result.Failed++
			continue
		}
		if protected {
			result.Protected++
			continue
		}
		if !eligible {
			continue
		}
		if removed {
			result.Removed++
			result.FreedBytes += candidate.bytes
		}
	}
	if !dryRun {
		if err := errors.Join(
			syncExistingLocalDirectory(jobs),
			syncExistingLocalDirectory(downloads),
		); err != nil {
			return result, err
		}
	}
	return result, nil
}

func syncExistingLocalDirectory(path string) error {
	err := syncLocalDirectory(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func collectLocalOutputCandidate(candidate *localOutputCandidate, cutoff time.Time, dryRun bool) (removed, eligible, protected bool, err error) {
	eligible = true
	var state localOutputState
	if candidate.statePath != "" {
		state, err = loadLocalOutputStateFile(candidate.statePath, candidate.key)
		if err != nil {
			return false, false, false, err
		}
		pending, pendingErr := localOutputTransactionExists(state)
		if pendingErr != nil {
			return false, false, false, pendingErr
		}
		if pending || (!state.Terminal && state.SubmissionStarted &&
			!candidate.modified.Before(time.Now().Add(-unresolvedOutputStateProtection))) {
			return false, true, true, nil
		}
	}
	remove := func() error {
		if candidate.statePath != "" {
			current, err := loadLocalOutputStateFile(candidate.statePath, candidate.key)
			if err != nil {
				return err
			}
			pending, pendingErr := localOutputTransactionExists(current)
			if pendingErr != nil {
				return pendingErr
			}
			if pending || (!current.Terminal && current.SubmissionStarted &&
				!candidate.modified.Before(time.Now().Add(-unresolvedOutputStateProtection))) {
				protected = true
				return nil
			}
		}
		expired, err := localOutputCandidateExpired(candidate, cutoff)
		if err != nil {
			return err
		}
		if !expired {
			eligible = false
			return nil
		}
		if dryRun {
			return nil
		}
		for _, downloadPath := range candidate.downloadPaths {
			if err := os.RemoveAll(downloadPath); err != nil {
				return err
			}
		}
		if candidate.statePath != "" {
			if err := os.Remove(candidate.statePath); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	}
	if candidate.statePath == "" {
		err = remove()
	} else if _, statErr := os.Stat(state.Root); statErr == nil {
		err = withWorkspaceOutputLock(state.Root, remove)
	} else if os.IsNotExist(statErr) {
		err = remove()
	} else {
		err = statErr
	}
	return err == nil && eligible && !protected, eligible, protected, err
}

func localOutputTransactionExists(state localOutputState) (bool, error) {
	if state.Pending == "" {
		return false, nil
	}
	exists, err := outputops.WorkspaceContainsApplyTransaction(state.Root, state.Pending, state.RootID)
	if os.IsNotExist(err) {
		return true, nil
	}
	return exists, err
}

func localOutputCandidateExpired(candidate *localOutputCandidate, cutoff time.Time) (bool, error) {
	paths := make([]string, 0, 1+len(candidate.downloadPaths))
	if candidate.statePath != "" {
		paths = append(paths, candidate.statePath)
	}
	paths = append(paths, candidate.downloadPaths...)
	for _, path := range paths {
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if !info.ModTime().Before(cutoff) {
			return false, nil
		}
	}
	return true, nil
}

// ReconcileCollectedJobOutputs replays durable runner collection markers so a
// lost GC response cannot strand local output state as unresolved.
func ReconcileCollectedJobOutputs(peerURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), maintenanceTimeout)
	defer cancel()
	clientID, err := localOutputClientID()
	if err != nil {
		return fmt.Errorf("loading local output client identity: %w", err)
	}
	cursor := ""
	for {
		var page proto.CollectedJobsPage
		endpoint := strings.TrimSuffix(peerURL, "/") + "/v0/jobs/collected?client_id=" + clientID
		if cursor != "" {
			endpoint += "&cursor=" + cursor
		}
		if err := getJSONWithClientContext(ctx, maintenanceHTTP, endpoint, 1<<20, "collected jobs", &page); err != nil {
			return err
		}
		if len(page.JobIDs) > proto.CollectedJobsPageLimit {
			return fmt.Errorf("collected jobs page exceeds %d IDs", proto.CollectedJobsPageLimit)
		}
		if err := reconcileCollectedJobIDs(peerURL, page.JobIDs); err != nil {
			return err
		}
		if len(page.JobIDs) > 0 {
			var acknowledged proto.CollectedJobsAckResult
			if err := postJSONResultContextTimeout(
				ctx, maintenanceHTTP, maintenanceTimeout,
				strings.TrimSuffix(peerURL, "/")+"/v0/jobs/collected/ack",
				proto.CollectedJobsAck{ClientID: clientID, JobIDs: page.JobIDs},
				"collection acknowledgement", &acknowledged,
			); err != nil {
				return err
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		if !proto.ValidULID(page.NextCursor) || page.NextCursor <= cursor {
			return fmt.Errorf("runner returned an invalid collection cursor")
		}
		cursor = page.NextCursor
	}
}

func reconcileCollectedJobIDs(peerURL string, jobIDs []string) error {
	root, err := localOutputRoot()
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(jobIDs))
	var failures []error
	for _, jobID := range jobIDs {
		if !proto.ValidULID(jobID) {
			failures = append(failures, fmt.Errorf("job GC returned invalid job ID %q", jobID))
			continue
		}
		if _, ok := seen[jobID]; ok {
			continue
		}
		seen[jobID] = struct{}{}
		key := localOutputKey(peerURL, jobID)
		unlock, lockErr := acquireLocalOutputLock(localOutputTransferLockName(key))
		if lockErr != nil {
			failures = append(failures, fmt.Errorf("reconciling %s: %w", jobID, lockErr))
			continue
		}
		reconcileErr := reconcileRemovedJobOutput(root, key)
		unlock()
		if reconcileErr != nil {
			failures = append(failures, fmt.Errorf("reconciling %s: %w", jobID, reconcileErr))
		}
	}
	return errors.Join(failures...)
}

func reconcileRemovedJobOutput(root, key string) error {
	statePath := filepath.Join(root, "jobs", key+".json")
	state, stateErr := loadLocalOutputStateFile(statePath, key)
	if os.IsNotExist(stateErr) {
		return nil
	}
	if stateErr != nil {
		return stateErr
	}
	settle := func() error {
		current, currentErr := loadLocalOutputStateFile(statePath, key)
		if os.IsNotExist(currentErr) {
			return nil
		}
		if currentErr != nil || current.Terminal {
			return currentErr
		}
		info, err := os.Stat(statePath)
		if err != nil {
			return err
		}
		current.Terminal = true
		if err := saveLocalOutputState(current); err != nil {
			return err
		}
		return os.Chtimes(statePath, info.ModTime(), info.ModTime())
	}
	_, statErr := os.Stat(state.Root)
	if statErr == nil {
		return withWorkspaceOutputLock(state.Root, settle)
	} else if os.IsNotExist(statErr) {
		return settle()
	}
	return statErr
}

func collectOutputGCCandidates(jobs, downloads string, candidates map[string]*localOutputCandidate) error {
	if entries, err := os.ReadDir(jobs); err == nil {
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			key := strings.TrimSuffix(entry.Name(), ".json")
			candidate := candidateFor(candidates, key)
			candidate.statePath = filepath.Join(jobs, entry.Name())
			candidate.modified = laterTime(candidate.modified, info.ModTime())
			candidate.bytes += info.Size()
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if entries, err := os.ReadDir(downloads); err == nil {
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			key := localOutputCandidateKey(entry.Name())
			candidate := candidateFor(candidates, key)
			downloadPath := filepath.Join(downloads, entry.Name())
			candidate.downloadPaths = append(candidate.downloadPaths, downloadPath)
			candidate.modified = laterTime(candidate.modified, info.ModTime())
			size, err := localTreeSize(downloadPath)
			if err != nil {
				return err
			}
			candidate.bytes += size
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func localOutputCandidateKey(name string) string {
	if validLocalOutputKey(name) {
		return name
	}
	if strings.HasPrefix(name, ".outputs-") {
		rest := strings.TrimPrefix(name, ".outputs-")
		if len(rest) > localOutputKeyLength && rest[localOutputKeyLength] == '-' {
			key := rest[:localOutputKeyLength]
			if validLocalOutputKey(key) {
				return key
			}
		}
	}
	return name
}

func validLocalOutputKey(key string) bool {
	if len(key) != localOutputKeyLength || key[32] != '-' || !proto.ValidULID(key[33:]) {
		return false
	}
	_, err := hex.DecodeString(key[:32])
	return err == nil
}

func candidateFor(candidates map[string]*localOutputCandidate, key string) *localOutputCandidate {
	if candidates[key] == nil {
		candidates[key] = &localOutputCandidate{key: key}
	}
	return candidates[key]
}

func localTreeSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func laterTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
