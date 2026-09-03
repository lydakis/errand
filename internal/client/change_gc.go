package client

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/proto"
)

type ChangeGCResult struct {
	Selected   int
	Removed    int
	Protected  int
	Failed     int
	FreedBytes int64
	DryRun     bool
}

// ChangeStats reports the local change state that is managed by gc changes.
// Bytes uses the same accounting as GC so the inventory and reclaimed-space
// reports remain comparable.
func ChangeStats() (proto.StorageCategory, error) {
	return changeStatsWithCollector(collectChangeGCCandidates)
}

func changeStatsWithCollector(
	collect func(string, string, map[string]*localChangeCandidate) error,
) (proto.StorageCategory, error) {
	root, err := localChangeRoot()
	if err != nil {
		return proto.StorageCategory{}, err
	}
	candidates := map[string]*localChangeCandidate{}
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

type localChangeCandidate struct {
	key            string
	statePath      string
	downloadPaths  []string
	modified       time.Time
	bytes          int64
	transferActive bool
	scanFailed     bool
}

const unresolvedChangeStateProtection = proto.ChangeReconciliationWindow

// ChangeGC removes old local workspace identity records and downloaded change staging.
// Pending apply transactions are always protected; unresolved submitted jobs
// are protected for the runner's bounded reconciliation window.
func ChangeGC(olderThan time.Duration, dryRun bool) (ChangeGCResult, error) {
	result := ChangeGCResult{DryRun: dryRun}
	if olderThan < time.Second {
		return result, fmt.Errorf("local change retention must be at least 1s")
	}
	root, err := localChangeRoot()
	if err != nil {
		return result, err
	}
	candidates := map[string]*localChangeCandidate{}
	jobs := filepath.Join(root, "jobs")
	downloads := filepath.Join(root, "downloads")
	collector := collectChangeGCCandidates
	if dryRun {
		collector = collectChangeGCCandidatesReadOnly
	}
	if err := collector(jobs, downloads, candidates); err != nil {
		return result, err
	}
	cutoff := time.Now().Add(-olderThan)
	for _, candidate := range candidates {
		if !candidate.modified.Before(cutoff) {
			continue
		}
		result.Selected++
		if dryRun {
			if candidate.scanFailed {
				result.Failed++
				continue
			}
			if candidate.transferActive {
				result.Protected++
				continue
			}
			unlock, acquired, lockErr := tryAcquireExistingLocalChangeLock(localChangeTransferLockName(candidate.key))
			if lockErr != nil {
				result.Failed++
				continue
			}
			if !acquired {
				result.Protected++
				continue
			}
			removed, eligible, protected, removeErr := collectLocalChangeCandidate(candidate, cutoff, true)
			unlock()
			if removeErr != nil {
				result.Failed++
				continue
			}
			if protected {
				result.Protected++
				continue
			}
			if removed && eligible {
				result.Removed++
				result.FreedBytes += candidate.bytes
			}
			continue
		}
		unlock, acquired, lockErr := tryAcquireLocalChangeLock(localChangeTransferLockName(candidate.key))
		if lockErr != nil {
			result.Failed++
			continue
		}
		if !acquired {
			result.Protected++
			continue
		}
		removed, eligible, protected, removeErr := collectLocalChangeCandidate(candidate, cutoff, dryRun)
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

func collectLocalChangeCandidate(candidate *localChangeCandidate, cutoff time.Time, dryRun bool) (removed, eligible, protected bool, err error) {
	eligible = true
	var state localChangeState
	if candidate.statePath != "" {
		state, err = loadLocalChangeStateFile(candidate.statePath, candidate.key)
		if err != nil {
			return false, false, false, err
		}
		pending, unavailable, pendingErr := localChangeTransactionExists(state)
		if pendingErr != nil {
			return false, false, false, pendingErr
		}
		if pending || localChangeStateNeedsProtection(state, unavailable, candidate.modified, time.Now()) {
			return false, true, true, nil
		}
	}
	remove := func() error {
		if candidate.statePath != "" {
			current, err := loadLocalChangeStateFile(candidate.statePath, candidate.key)
			if err != nil {
				return err
			}
			pending, unavailable, pendingErr := localChangeTransactionExists(current)
			if pendingErr != nil {
				return pendingErr
			}
			if pending || localChangeStateNeedsProtection(current, unavailable, candidate.modified, time.Now()) {
				protected = true
				return nil
			}
		}
		expired, err := localChangeCandidateExpired(candidate, cutoff)
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
			if err := changeops.RemoveTree(downloadPath); err != nil {
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
	if dryRun || candidate.statePath == "" {
		err = remove()
	} else if _, statErr := os.Stat(state.Root); statErr == nil {
		err = withWorkspaceChangeLock(state.Root, remove)
	} else if os.IsNotExist(statErr) {
		err = remove()
	} else {
		err = statErr
	}
	return err == nil && eligible && !protected, eligible, protected, err
}

func localChangeStateNeedsProtection(
	state localChangeState,
	transactionUnavailable bool,
	modified, now time.Time,
) bool {
	if modified.Before(now.Add(-unresolvedChangeStateProtection)) {
		return false
	}
	if transactionUnavailable || (!state.Terminal && state.SubmissionStarted) {
		return true
	}
	return state.ApplyOnSuccess && !automaticApplyFinished(state.AutomaticApply)
}

func localChangeTransactionExists(state localChangeState) (exists, unavailable bool, err error) {
	if state.Pending == "" {
		return false, false, nil
	}
	exists, err = changeops.WorkspaceContainsApplyTransaction(state.Root, state.Pending, state.RootID)
	if os.IsNotExist(err) {
		return false, true, nil
	}
	return exists, false, err
}

func localChangeCandidateExpired(candidate *localChangeCandidate, cutoff time.Time) (bool, error) {
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

// ReconcileCollectedJobChanges replays durable runner collection markers so a
// lost GC response cannot strand local change state as unresolved.
func ReconcileCollectedJobChanges(peerURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), maintenanceTimeout)
	defer cancel()
	clientID, err := localChangeClientID()
	if err != nil {
		return fmt.Errorf("loading local change client identity: %w", err)
	}
	cursor := ""
	for {
		var page proto.ChangeReconciliationPage
		endpoint := strings.TrimSuffix(peerURL, "/") + "/v0/change-reconciliation?client_id=" + clientID
		if cursor != "" {
			endpoint += "&cursor=" + cursor
		}
		if err := getJSONWithClientContext(ctx, maintenanceHTTP, endpoint, 1<<20, "change reconciliation", &page); err != nil {
			return err
		}
		if len(page.JobIDs) > proto.ChangeReconciliationPageLimit {
			return fmt.Errorf("change reconciliation page exceeds %d IDs", proto.ChangeReconciliationPageLimit)
		}
		if err := reconcileCollectedJobIDs(peerURL, page.JobIDs); err != nil {
			return err
		}
		if len(page.JobIDs) > 0 {
			var acknowledged proto.ChangeReconciliationAckResult
			if err := postJSONResultContextTimeout(
				ctx, maintenanceHTTP, maintenanceTimeout,
				strings.TrimSuffix(peerURL, "/")+"/v0/change-reconciliation/ack",
				proto.ChangeReconciliationAck{ClientID: clientID, JobIDs: page.JobIDs},
				"change reconciliation acknowledgement", &acknowledged,
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
	root, err := localChangeRoot()
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
		key := localChangeKey(peerURL, jobID)
		unlock, lockErr := acquireLocalChangeLock(localChangeTransferLockName(key))
		if lockErr != nil {
			failures = append(failures, fmt.Errorf("reconciling %s: %w", jobID, lockErr))
			continue
		}
		reconcileErr := reconcileRemovedJobChange(root, key)
		unlock()
		if reconcileErr != nil {
			failures = append(failures, fmt.Errorf("reconciling %s: %w", jobID, reconcileErr))
		}
	}
	return errors.Join(failures...)
}

func reconcileRemovedJobChange(root, key string) error {
	statePath := filepath.Join(root, "jobs", key+".json")
	state, stateErr := loadLocalChangeStateFile(statePath, key)
	if os.IsNotExist(stateErr) {
		return nil
	}
	if stateErr != nil {
		return stateErr
	}
	settle := func() error {
		current, currentErr := loadLocalChangeStateFile(statePath, key)
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
		if err := saveLocalChangeState(current); err != nil {
			return err
		}
		return os.Chtimes(statePath, info.ModTime(), info.ModTime())
	}
	_, statErr := os.Stat(state.Root)
	if statErr == nil {
		return withWorkspaceChangeLock(state.Root, settle)
	} else if os.IsNotExist(statErr) {
		return settle()
	}
	return statErr
}

func collectChangeGCCandidates(jobs, downloads string, candidates map[string]*localChangeCandidate) error {
	return collectChangeGCCandidatesMode(jobs, downloads, candidates, false)
}

func collectChangeGCCandidatesReadOnly(jobs, downloads string, candidates map[string]*localChangeCandidate) error {
	return collectChangeGCCandidatesMode(jobs, downloads, candidates, true)
}

func collectChangeGCCandidatesMode(
	jobs, downloads string,
	candidates map[string]*localChangeCandidate,
	readOnly bool,
) error {
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
			key := localChangeCandidateKey(entry.Name())
			candidate := candidateFor(candidates, key)
			downloadPath := filepath.Join(downloads, entry.Name())
			candidate.downloadPaths = append(candidate.downloadPaths, downloadPath)
			candidate.modified = laterTime(candidate.modified, info.ModTime())
			var unlock func()
			if readOnly {
				var acquired bool
				unlock, acquired, err = tryAcquireExistingLocalChangeLock(localChangeTransferLockName(key))
				if err != nil {
					candidate.scanFailed = true
					continue
				}
				if !acquired {
					candidate.transferActive = true
					continue
				}
			} else {
				unlock, err = acquireLocalChangeLock(localChangeTransferLockName(key))
				if err != nil {
					return err
				}
			}
			var size int64
			if readOnly {
				size, err = readOnlyTreeSize(downloadPath)
			} else {
				size, err = changeops.TreeSize(downloadPath)
			}
			unlock()
			if err != nil {
				if readOnly {
					candidate.scanFailed = true
					continue
				}
				return err
			}
			candidate.bytes += size
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func readOnlyTreeSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, walkErr error) error {
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

func localChangeCandidateKey(name string) string {
	if validLocalChangeKey(name) {
		return name
	}
	if strings.HasPrefix(name, ".changes-") {
		rest := strings.TrimPrefix(name, ".changes-")
		if len(rest) > localChangeKeyLength && rest[localChangeKeyLength] == '-' {
			key := rest[:localChangeKeyLength]
			if validLocalChangeKey(key) {
				return key
			}
		}
	}
	return name
}

func validLocalChangeKey(key string) bool {
	if len(key) != localChangeKeyLength || key[32] != '-' || !proto.ValidULID(key[33:]) {
		return false
	}
	_, err := hex.DecodeString(key[:32])
	return err == nil
}

func candidateFor(candidates map[string]*localChangeCandidate, key string) *localChangeCandidate {
	if candidates[key] == nil {
		candidates[key] = &localChangeCandidate{key: key}
	}
	return candidates[key]
}

func laterTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
