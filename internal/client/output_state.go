package client

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lydakis/errand/internal/fsidentity"
	outputops "github.com/lydakis/errand/internal/outputs"
	"github.com/lydakis/errand/internal/proto"
)

const localOutputStateVersion = 1

func localOutputClientID() (string, error) {
	root, err := localOutputRoot()
	if err != nil {
		return "", err
	}
	if err := ensurePrivateLocalDirectory(root); err != nil {
		return "", err
	}
	unlock, err := acquireLocalOutputLock("client-id")
	if err != nil {
		return "", err
	}
	defer unlock()
	path := filepath.Join(root, "client-id")
	read := func() (string, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		id := strings.TrimSpace(string(raw))
		if !proto.ValidOutputClientID(id) {
			return "", fmt.Errorf("invalid local output client ID")
		}
		return id, nil
	}
	if id, err := read(); err == nil {
		return id, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	removedTemporary := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), ".client-id-") {
			continue
		}
		if err := os.Remove(filepath.Join(root, entry.Name())); err != nil && !os.IsNotExist(err) {
			return "", err
		}
		removedTemporary = true
	}
	if removedTemporary {
		if err := syncLocalDirectory(root); err != nil {
			return "", err
		}
	}

	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	id := hex.EncodeToString(random)
	f, err := os.CreateTemp(root, ".client-id-")
	if err != nil {
		return "", err
	}
	tmpName := f.Name()
	defer os.Remove(tmpName)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return "", err
	}
	if _, err := f.WriteString(id + "\n"); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", err
	}
	if err := syncLocalDirectory(root); err != nil {
		return "", err
	}
	return id, nil
}

type localOutputState struct {
	Version           int                  `json:"version"`
	JobID             string               `json:"job_id"`
	PeerURL           string               `json:"peer_url"`
	Root              string               `json:"root"`
	RootID            fsidentity.Identity  `json:"root_identity"`
	Outputs           []proto.OutputSpec   `json:"outputs"`
	Baselines         []outputops.Baseline `json:"baselines"`
	SubmissionStarted bool                 `json:"submission_started,omitempty"`
	Terminal          bool                 `json:"terminal,omitempty"`
	Applied           map[string]string    `json:"applied,omitempty"`
	Pending           string               `json:"pending_transaction,omitempty"`
}

func loadLocalOutputState(peerURL, jobID string) (localOutputState, error) {
	var state localOutputState
	if !proto.ValidULID(jobID) {
		return state, fmt.Errorf("invalid job ID %q", jobID)
	}
	root, err := localOutputRoot()
	if err != nil {
		return state, err
	}
	peerURL = strings.TrimSuffix(peerURL, "/")
	raw, err := os.ReadFile(filepath.Join(root, "jobs", localOutputKey(peerURL, jobID)+".json"))
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, err
	}
	if state.Version != localOutputStateVersion || state.JobID != jobID || state.PeerURL != peerURL {
		return state, fmt.Errorf("local output state does not match this job")
	}
	return state, nil
}

func loadLocalOutputStateFile(path, owner string) (localOutputState, error) {
	var state localOutputState
	raw, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, err
	}
	if state.Version != localOutputStateVersion || !proto.ValidULID(state.JobID) ||
		localOutputKey(state.PeerURL, state.JobID) != owner {
		return state, fmt.Errorf("local output state identity mismatch")
	}
	return state, nil
}

func saveLocalOutputState(state localOutputState) error {
	if !proto.ValidULID(state.JobID) {
		return fmt.Errorf("invalid job ID %q", state.JobID)
	}
	state.PeerURL = strings.TrimSuffix(state.PeerURL, "/")
	if state.RootID.IsZero() {
		identity, info, err := fsidentity.Lstat(state.Root)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("local output workspace root is not a directory")
		}
		state.RootID = identity
	}
	root, err := localOutputRoot()
	if err != nil {
		return err
	}
	jobs := filepath.Join(root, "jobs")
	if err := ensurePrivateLocalDirectory(root); err != nil {
		return err
	}
	if err := ensurePrivateLocalDirectory(jobs); err != nil {
		return err
	}
	dest := filepath.Join(jobs, localOutputKey(state.PeerURL, state.JobID)+".json")
	tmp, err := os.CreateTemp(jobs, ".job-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := json.NewEncoder(tmp).Encode(state); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return err
	}
	return syncLocalDirectory(jobs)
}

func discardUnsubmittedOutputState(peerURL, jobID string) error {
	key := localOutputKey(peerURL, jobID)
	unlock, err := acquireLocalOutputLock(localOutputTransferLockName(key))
	if err != nil {
		return err
	}
	defer unlock()
	state, err := loadLocalOutputState(peerURL, jobID)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	remove := func() error {
		current, err := loadLocalOutputState(peerURL, jobID)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.Terminal || current.Pending != "" || len(current.Applied) != 0 {
			return fmt.Errorf("output state is no longer unsubmitted")
		}
		root, err := localOutputRoot()
		if err != nil {
			return err
		}
		jobs := filepath.Join(root, "jobs")
		if err := os.Remove(filepath.Join(jobs, key+".json")); err != nil && !os.IsNotExist(err) {
			return err
		}
		return syncLocalDirectory(jobs)
	}
	if _, err := os.Stat(state.Root); err == nil {
		return withWorkspaceOutputLock(state.Root, remove)
	} else if os.IsNotExist(err) {
		return remove()
	} else {
		return err
	}
}

func markLocalOutputTerminal(peerURL, jobID string) error {
	unlock, err := acquireLocalOutputLock(localOutputTransferLockName(localOutputKey(peerURL, jobID)))
	if err != nil {
		return err
	}
	defer unlock()
	state, err := loadLocalOutputState(peerURL, jobID)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	mark := func() error {
		state, err = loadLocalOutputState(peerURL, jobID)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil || state.Terminal {
			return err
		}
		state.Terminal = true
		return saveLocalOutputState(state)
	}
	if _, err := os.Stat(state.Root); err == nil {
		return withWorkspaceOutputLock(state.Root, mark)
	} else if os.IsNotExist(err) {
		return mark()
	} else {
		return err
	}
}

func markLocalOutputSubmissionStarted(peerURL, jobID string) error {
	unlock, err := acquireLocalOutputLock(localOutputTransferLockName(localOutputKey(peerURL, jobID)))
	if err != nil {
		return err
	}
	defer unlock()
	state, err := loadLocalOutputState(peerURL, jobID)
	if err != nil {
		return err
	}
	mark := func() error {
		state, err = loadLocalOutputState(peerURL, jobID)
		if err != nil || state.SubmissionStarted {
			return err
		}
		state.SubmissionStarted = true
		return saveLocalOutputState(state)
	}
	if _, err := os.Stat(state.Root); err == nil {
		return withWorkspaceOutputLock(state.Root, mark)
	} else if os.IsNotExist(err) {
		return mark()
	} else {
		return err
	}
}

func recoverWorkspaceApplications(root string) error {
	return recoverWorkspaceApplicationsContext(context.Background(), root)
}

func recoverWorkspaceApplicationsContext(ctx context.Context, root string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pending, err := outputops.WorkspaceHasApplyTransactions(root)
	if err != nil {
		return err
	}
	if !pending {
		return nil
	}
	stateRoot, err := localOutputRoot()
	if err != nil {
		return err
	}
	jobs := filepath.Join(stateRoot, "jobs")
	entries, err := os.ReadDir(jobs)
	if os.IsNotExist(err) {
		return fmt.Errorf("workspace contains an output apply transaction with no matching local output state")
	}
	if err != nil {
		return err
	}
	var invalidStates []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		owner := strings.TrimSuffix(entry.Name(), ".json")
		state, err := loadLocalOutputStateFile(filepath.Join(jobs, entry.Name()), owner)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			invalidStates = append(invalidStates, fmt.Errorf("loading %s: %w", entry.Name(), err))
			continue
		}
		if state.Pending == "" || !sameLocalRoot(state.Root, root) {
			continue
		}
		if _, err := recoverOneApplicationContext(ctx, &state, owner); err != nil {
			return err
		}
	}
	pending, err = outputops.WorkspaceHasApplyTransactions(root)
	if err != nil {
		return err
	}
	if pending {
		if len(invalidStates) > 0 {
			return invalidStates[0]
		}
		return fmt.Errorf("workspace contains an output apply transaction with no matching local output state")
	}
	return nil
}

func recoverOneApplication(state *localOutputState, owner string) ([]string, error) {
	return recoverOneApplicationContext(context.Background(), state, owner)
}

func recoverOneApplicationContext(ctx context.Context, state *localOutputState, owner string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if state.Pending == "" {
		return nil, nil
	}
	if err := validateLocalWorkspaceIdentity(*state); err != nil {
		return nil, err
	}
	pending, err := outputops.RecoverApplicationToWorkspaceContext(ctx, state.Root, state.Pending, state.RootID)
	if err != nil {
		return nil, err
	}
	if pending == nil {
		state.Pending = ""
		return nil, saveLocalOutputState(*state)
	}
	if pending.Owner != owner {
		return nil, fmt.Errorf("output transaction owner mismatch")
	}
	if state.Applied == nil {
		state.Applied = map[string]string{}
	}
	for _, outputPath := range pending.Paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		state.Applied[outputPath] = pending.BundleRoot
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := saveLocalOutputState(*state); err != nil {
		return nil, err
	}
	if err := outputops.CommitApplyToWorkspace(state.Root, pending.Transaction, state.RootID); err != nil {
		return nil, err
	}
	state.Pending = ""
	if err := saveLocalOutputState(*state); err != nil {
		return nil, err
	}
	return pending.Paths, nil
}

func withWorkspaceOutputLock(root string, fn func() error) error {
	return withWorkspaceOutputLockContext(context.Background(), root, fn)
}

func withWorkspaceOutputLockContext(ctx context.Context, root string, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	} else {
		return err
	}
	unlock, err := acquireLocalOutputLockContext(ctx, localOutputWorkspaceLockName(filepath.Clean(abs)))
	if err != nil {
		return err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func validateLocalWorkspaceIdentity(state localOutputState) error {
	identity, info, err := fsidentity.Lstat(state.Root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || identity != state.RootID {
		return fmt.Errorf("local output workspace at %q is not the workspace that submitted job %s", state.Root, state.JobID)
	}
	return nil
}

func acquireLocalOutputLock(name string) (func(), error) {
	return acquireLocalOutputLockContext(context.Background(), name)
}

func openLocalOutputLock(name string) (*os.File, error) {
	root, err := localOutputRoot()
	if err != nil {
		return nil, err
	}
	locks := filepath.Join(root, "locks")
	if err := ensurePrivateLocalDirectory(root); err != nil {
		return nil, err
	}
	if err := ensurePrivateLocalDirectory(locks); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(locks, name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
}

func acquireLocalOutputLockContext(ctx context.Context, name string) (func(), error) {
	f, err := openLocalOutputLock(name)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func tryAcquireLocalOutputLock(name string) (func(), bool, error) {
	f, err := openLocalOutputLock(name)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true, nil
}

const localOutputLockStripes = 256

func localOutputTransferLockName(key string) string {
	return localOutputStripedLockName("download", key)
}

func localOutputWorkspaceLockName(root string) string {
	return localOutputStripedLockName("workspace", root)
}

func localOutputStripedLockName(kind, key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%s-%02x", kind, sum[0])
}

func sameLocalRoot(a, b string) bool {
	aAbs, aErr := filepath.Abs(a)
	bAbs, bErr := filepath.Abs(b)
	if aErr != nil || bErr != nil {
		return false
	}
	return filepath.Clean(aAbs) == filepath.Clean(bAbs)
}

func ensurePrivateLocalDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return syncLocalDirectory(filepath.Dir(path))
}

func localOutputRoot() (string, error) {
	if stateHome := os.Getenv("XDG_STATE_HOME"); stateHome != "" {
		if !filepath.IsAbs(stateHome) {
			return "", fmt.Errorf("XDG_STATE_HOME must be an absolute path")
		}
		return filepath.Join(stateHome, "errand"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "errand"), nil
}

const localOutputKeyLength = 32 + 1 + 26

func localOutputKey(peerURL, jobID string) string {
	peerURL = strings.TrimSuffix(peerURL, "/")
	sum := sha256.Sum256([]byte(peerURL))
	return hex.EncodeToString(sum[:16]) + "-" + jobID
}

func writeLocalJSON(path string, value any) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(value)
	if syncErr := f.Sync(); err == nil {
		err = syncErr
	}
	return errors.Join(err, f.Close())
}

func syncLocalDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
