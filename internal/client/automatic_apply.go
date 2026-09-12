package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

const (
	automaticApplyPending   = "pending"
	automaticApplyRunning   = "applying"
	automaticApplyApplied   = "applied"
	automaticApplyNoChanges = "no_changes"
	automaticApplySkipped   = "skipped"
	automaticApplyFailed    = "failed"

	automaticApplyPollInterval = 2 * time.Second
	automaticApplyMaxBackoff   = 30 * time.Second
)

var (
	launchAutomaticApplyWorker     = startAutomaticApplyWorkerProcess
	confirmAutomaticApplyAdmission = markLocalChangeAdmissionConfirmed
)

type automaticApplyOutcome struct {
	state  string
	staged string
	err    string
}

// AutomaticApplyStatus combines the originating client's saved apply policy
// with observed worker ownership. AutomaticApplyNeedsRecovery is derived, never persisted.
// It is absent on clients that did not submit the job.
type AutomaticApplyStatus struct {
	State    string `json:"state"`
	Error    string `json:"error,omitempty"`
	StagedAt string `json:"staged_at,omitempty"`
}

func handoffAutomaticApply(peerURL, jobID string) error {
	if err := launchAutomaticApplyWorker(peerURL, jobID); err != nil {
		outcome := automaticApplyOutcome{state: automaticApplyPending, err: err.Error()}
		if recordErr := recordAutomaticApply(peerURL, jobID, outcome); recordErr != nil {
			return errors.Join(err, recordErr)
		}
		return err
	}
	return nil
}

func startAutomaticApplyWorkerProcess(peerURL, jobID string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer null.Close()

	cmd := exec.Command(executable, "_automatic-apply", strings.TrimSuffix(peerURL, "/"), jobID)
	stateRoot, err := localChangeRoot()
	if err != nil {
		return err
	}
	cmd.Dir = stateRoot
	cmd.Env = automaticApplyWorkerEnvironment()
	cmd.Stdin = null
	cmd.Stdout = null
	cmd.Stderr = null
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	return nil
}

func automaticApplyWorkerEnvironment() []string {
	allowed := []string{"HOME", "XDG_STATE_HOME", "PATH", "TMPDIR", "LANG", "LC_ALL"}
	env := make([]string, 0, len(allowed))
	for _, name := range allowed {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// RunAutomaticApplyWorker completes the apply policy already recorded for a
// submitted job. It is invoked by a detached copy of the errand executable.
func RunAutomaticApplyWorker(peerURL, jobID string) error {
	key := localChangeKey(peerURL, jobID)
	unlock, acquired, err := tryAcquireLocalChangeLease(localAutomaticApplyWorkerLockName(key))
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	defer unlock()
	return runAutomaticApplyWorkerContext(context.Background(), peerURL, jobID, automaticApplyPollInterval)
}

func runAutomaticApplyWorkerContext(ctx context.Context, peerURL, jobID string, pollInterval time.Duration) error {
	consecutiveErrors := 0
	now := time.Now()
	tracker := newStreamDeadlineTracker(now, proto.JobStatus{State: proto.StateStaging})
	unconfirmedDeadline := now.Add(submitRequestTimeout)
	for {
		state, err := loadLocalChangeState(peerURL, jobID)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		restoreSSHEndpoint(state)
		if !state.ApplyOnSuccess || automaticApplyFinished(state.AutomaticApply) {
			return nil
		}

		statusCtx, cancel := context.WithTimeout(ctx, controlRequestTimeout)
		status, statusErr := getStatusContext(statusCtx, peerURL, jobID)
		cancel()
		now = time.Now()
		if statusErr == nil {
			tracker.observe(now, status)
			if !state.AdmissionConfirmed {
				if err := markLocalChangeAdmissionConfirmed(peerURL, jobID); err != nil {
					return err
				}
				state.AdmissionConfirmed = true
			}
			if status.Result != nil {
				outcome, applyErr := applyTerminalAutomatically(peerURL, jobID, status)
				if applyErr == nil || outcome.state != automaticApplyPending {
					return applyErr
				}
				statusErr = applyErr
			}
		}
		if statusErr != nil && automaticApplyStatusErrorIsPermanent(statusErr, state.AdmissionConfirmed) {
			recordErr := recordAutomaticApply(peerURL, jobID, automaticApplyOutcome{
				state: automaticApplyFailed, err: statusErr.Error(),
			})
			return errors.Join(statusErr, recordErr)
		}
		if statusErr != nil {
			consecutiveErrors++
		} else {
			consecutiveErrors = 0
		}
		if (!state.AdmissionConfirmed && now.After(unconfirmedDeadline)) || now.After(tracker.deadline) {
			waitErr := fmt.Errorf("automatic apply could not observe job completion before its transaction deadline")
			if statusErr != nil {
				waitErr = errors.Join(waitErr, statusErr)
			}
			recordErr := recordAutomaticApply(peerURL, jobID, automaticApplyOutcome{
				state: automaticApplyPending, err: waitErr.Error(),
			})
			return errors.Join(waitErr, recordErr)
		}

		timer := time.NewTimer(automaticApplyPollDelay(pollInterval, consecutiveErrors))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func restoreSSHEndpoint(state localChangeState) {
	restoreSSHPeer(state.PeerURL, state.SSHTarget, state.SSHRemoteCommand, state.SSHRemoteSocket)
}

func automaticApplyPollDelay(base time.Duration, consecutiveErrors int) time.Duration {
	delay := base
	for range min(consecutiveErrors, 4) {
		if delay >= automaticApplyMaxBackoff/2 {
			return automaticApplyMaxBackoff
		}
		delay *= 2
	}
	return min(delay, automaticApplyMaxBackoff)
}

func automaticApplyStatusErrorIsPermanent(err error, admissionConfirmed bool) bool {
	var responseErr *controlHTTPError
	if !errors.As(err, &responseErr) {
		return false
	}
	if responseErr.statusCode == http.StatusNotFound {
		return admissionConfirmed
	}
	return !responseErr.retryableDuringAdmission(false)
}

func automaticApplyFinished(state string) bool {
	switch state {
	case automaticApplyApplied, automaticApplyNoChanges, automaticApplySkipped, automaticApplyFailed:
		return true
	default:
		return false
	}
}

func applyTerminalAutomatically(peerURL, jobID string, final proto.JobStatus) (automaticApplyOutcome, error) {
	key := localChangeKey(peerURL, jobID)
	unlock, err := acquireLocalChangeLock(localAutomaticApplyLockName(key))
	if err != nil {
		return automaticApplyOutcome{}, err
	}
	defer unlock()
	state, err := loadLocalChangeState(peerURL, jobID)
	if err != nil {
		return automaticApplyOutcome{}, err
	}
	if automaticApplyFinished(state.AutomaticApply) {
		return automaticApplyOutcome{
			state: state.AutomaticApply, staged: state.AutomaticApplyDir, err: state.AutomaticApplyErr,
		}, nil
	}
	return applyTerminalAutomaticallyOwned(peerURL, jobID, final)
}

func applyTerminalAutomaticallyOwned(peerURL, jobID string, final proto.JobStatus) (automaticApplyOutcome, error) {
	if err := markLocalChangeTerminal(peerURL, jobID); err != nil {
		return automaticApplyOutcome{}, err
	}
	handle := peerLabel("", peerURL) + "/" + jobID
	if exitCode(final, io.Discard, handle) != 0 {
		outcome := automaticApplyOutcome{state: automaticApplySkipped}
		return outcome, recordAutomaticApply(peerURL, jobID, outcome)
	}
	checkpointResult := false
	if final.Result != nil && final.Result.Changes == nil {
		state, err := loadLocalChangeState(peerURL, jobID)
		if err != nil {
			return automaticApplyOutcome{}, err
		}
		checkpointResult = state.WorkspaceID != ""
	}
	if final.Result == nil || final.Result.Changes == nil && !checkpointResult {
		outcome := automaticApplyOutcome{state: automaticApplyNoChanges}
		return outcome, recordAutomaticApply(peerURL, jobID, outcome)
	}
	if err := recordAutomaticApply(peerURL, jobID, automaticApplyOutcome{state: automaticApplyRunning}); err != nil {
		return automaticApplyOutcome{}, err
	}
	state, err := loadLocalChangeState(peerURL, jobID)
	if err != nil {
		return automaticApplyOutcome{}, err
	}
	if automaticApplyFinished(state.AutomaticApply) {
		return automaticApplyOutcome{
			state: state.AutomaticApply, staged: state.AutomaticApplyDir, err: state.AutomaticApplyErr,
		}, nil
	}
	staged, err := FetchChanges(ChangeFetchOptions{
		PeerURL: peerURL, JobID: jobID, Apply: true, CallerDir: state.Root,
	})
	if err != nil {
		failureState := automaticApplyFailed
		if automaticApplyErrorIsTransient(err) {
			failureState = automaticApplyPending
		}
		outcome := automaticApplyOutcome{state: failureState, staged: staged, err: err.Error()}
		if recordErr := recordAutomaticApply(peerURL, jobID, outcome); recordErr != nil {
			return outcome, errors.Join(err, recordErr)
		}
		return outcome, err
	}
	outcome := automaticApplyOutcome{state: automaticApplyApplied, staged: staged}
	return outcome, recordAutomaticApply(peerURL, jobID, outcome)
}

func automaticApplyErrorIsTransient(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return true
	}
	var idleErr *streamIdleError
	if errors.As(err, &idleErr) {
		return true
	}
	var responseErr *controlHTTPError
	if !errors.As(err, &responseErr) {
		return false
	}
	switch responseErr.statusCode {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func automaticApplyForJob(peerURL, jobID string) (automaticApplyOutcome, bool, error) {
	state, err := loadLocalChangeState(peerURL, jobID)
	if os.IsNotExist(err) {
		return automaticApplyOutcome{}, false, nil
	}
	if err != nil {
		return automaticApplyOutcome{}, false, err
	}
	if !state.ApplyOnSuccess {
		return automaticApplyOutcome{}, false, nil
	}
	return automaticApplyOutcome{
		state: state.AutomaticApply, staged: state.AutomaticApplyDir, err: state.AutomaticApplyErr,
	}, true, nil
}

func GetAutomaticApplyStatus(peerURL, jobID string) (*AutomaticApplyStatus, error) {
	state, err := loadLocalChangeState(peerURL, jobID)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return inspectAutomaticApply(state)
}

func recordAutomaticApply(peerURL, jobID string, outcome automaticApplyOutcome) error {
	key := localChangeKey(peerURL, jobID)
	unlock, err := acquireLocalChangeLock(localChangeTransferLockName(key))
	if err != nil {
		return err
	}
	defer unlock()
	state, err := loadLocalChangeState(peerURL, jobID)
	if err != nil {
		return err
	}
	if !state.ApplyOnSuccess {
		return fmt.Errorf("job %s does not request automatic apply", jobID)
	}
	if automaticApplyFinished(state.AutomaticApply) {
		return nil
	}
	state.AutomaticApply = outcome.state
	state.AutomaticApplyDir = outcome.staged
	state.AutomaticApplyErr = boundedAutomaticApplyError(outcome.err)
	return saveLocalChangeState(state)
}

func boundedAutomaticApplyError(value string) string {
	const max = 4096
	if len(value) <= max {
		return value
	}
	return value[:max]
}
