package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/lydakis/errand/internal/proto"
)

// AutomaticApplyInspection describes a locally requested automatic apply.
// It is a point-in-time observation, not a change to the persisted policy.
type AutomaticApplyInspection struct {
	PeerURL     string
	JobID       string
	Root        string
	WorkspaceID string
	Status      AutomaticApplyStatus
}

const AutomaticApplyNeedsRecovery = "needs_recovery"

func (s AutomaticApplyStatus) NeedsAttention() bool {
	return s.State == AutomaticApplyNeedsRecovery || s.State == automaticApplyFailed
}

func inspectAutomaticApply(state localChangeState) (*AutomaticApplyStatus, error) {
	if !state.ApplyOnSuccess {
		return nil, nil
	}
	status := &AutomaticApplyStatus{State: state.AutomaticApply, Error: state.AutomaticApplyErr, StagedAt: state.AutomaticApplyDir}
	if !state.SubmissionStarted || automaticApplyFinished(state.AutomaticApply) {
		return status, nil
	}
	// Probe only an existing lock. Never create state or unlink the worker's
	// lease during inspection. Release immediately so the worker can proceed.
	active, err := automaticApplyWorkerActive(state.PeerURL, state.JobID)
	if err != nil {
		return nil, err
	}
	if active {
		return status, nil
	}
	// A worker may have finished between the state read and the lock probe.
	fresh, err := loadLocalChangeState(state.PeerURL, state.JobID)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	status.State, status.Error, status.StagedAt = fresh.AutomaticApply, fresh.AutomaticApplyErr, fresh.AutomaticApplyDir
	if !automaticApplyFinished(fresh.AutomaticApply) {
		// A worker can also acquire its lease while we reload the record.
		active, err = automaticApplyWorkerActive(state.PeerURL, state.JobID)
		if err != nil {
			return nil, err
		}
		if !active {
			status.State = AutomaticApplyNeedsRecovery
		}
	}
	return status, nil
}

func automaticApplyWorkerActive(peerURL, jobID string) (bool, error) {
	root, err := localChangeRoot()
	if err != nil {
		return false, err
	}
	name := localAutomaticApplyWorkerLockName(localChangeKey(peerURL, jobID))
	f, err := os.Open(filepath.Join(root, "locks", name+".lock"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	// Shared inspection locks are distinguishable from an exclusive worker
	// lease, so a starting worker waits for readers rather than exiting.
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return true, nil
	}
	return false, err
}

// InterruptedAutomaticApplies reads local intent and worker ownership without
// contacting runners, launching workers, or modifying the state directory.
func InterruptedAutomaticApplies() ([]AutomaticApplyInspection, error) {
	all, err := InspectAutomaticApplies()
	issues := all[:0]
	for _, item := range all {
		if item.Status.NeedsAttention() {
			issues = append(issues, item)
		}
	}
	return issues, err
}

// InspectAutomaticApplies reads each local record once, then observes worker
// ownership. A second state read after an inactive lease handles completion races.
func InspectAutomaticApplies() ([]AutomaticApplyInspection, error) {
	root, err := localChangeRoot()
	if err != nil {
		return nil, err
	}
	jobs := filepath.Join(root, "jobs")
	entries, err := os.ReadDir(jobs)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var issues []AutomaticApplyInspection
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		owner := strings.TrimSuffix(entry.Name(), ".json")
		if !validLocalChangeKey(owner) {
			continue
		}
		path := filepath.Join(jobs, entry.Name())
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		var state localChangeState
		if err == nil {
			err = json.Unmarshal(raw, &state)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("reading %s: %w", entry.Name(), err))
			continue
		}
		if !state.ApplyOnSuccess {
			continue
		}
		if !proto.ValidULID(state.JobID) || !validLocalManifestRoot(state.ManifestRoot) || localChangeKey(state.PeerURL, state.JobID) != owner {
			errs = append(errs, fmt.Errorf("local change state identity mismatch: %s", entry.Name()))
			continue
		}
		if !state.SubmissionStarted {
			continue
		}
		status, err := inspectAutomaticApply(state)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if status != nil {
			issues = append(issues, AutomaticApplyInspection{PeerURL: state.PeerURL, JobID: state.JobID, Root: state.Root, WorkspaceID: state.WorkspaceID, Status: *status})
		}
	}
	return issues, errors.Join(errs...)
}
