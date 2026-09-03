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

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
)

func localChangeClientID() (string, error) {
	root, err := localChangeRoot()
	if err != nil {
		return "", err
	}
	if err := ensurePrivateLocalDirectory(root); err != nil {
		return "", err
	}
	unlock, err := acquireLocalChangeLock("client-id")
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
		if !proto.ValidChangeClientID(id) {
			return "", fmt.Errorf("invalid local change client ID")
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

type localChangeState struct {
	JobID              string              `json:"job_id"`
	PeerURL            string              `json:"peer_url"`
	Root               string              `json:"root"`
	RootID             fsidentity.Identity `json:"root_identity"`
	ManifestRoot       string              `json:"manifest_root"`
	SubmissionStarted  bool                `json:"submission_started,omitempty"`
	AdmissionConfirmed bool                `json:"admission_confirmed,omitempty"`
	Terminal           bool                `json:"terminal,omitempty"`
	ApplyOnSuccess     bool                `json:"apply_on_success,omitempty"`
	AutomaticApply     string              `json:"automatic_apply,omitempty"`
	AutomaticApplyErr  string              `json:"automatic_apply_error,omitempty"`
	AutomaticApplyDir  string              `json:"automatic_apply_staged_at,omitempty"`
	Applied            map[string]string   `json:"applied,omitempty"`
	Pending            string              `json:"pending_transaction,omitempty"`
}

func loadLocalChangeState(peerURL, jobID string) (localChangeState, error) {
	var state localChangeState
	if !proto.ValidULID(jobID) {
		return state, fmt.Errorf("invalid job ID %q", jobID)
	}
	root, err := localChangeRoot()
	if err != nil {
		return state, err
	}
	peerURL = strings.TrimSuffix(peerURL, "/")
	raw, err := os.ReadFile(filepath.Join(root, "jobs", localChangeKey(peerURL, jobID)+".json"))
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, err
	}
	if state.JobID != jobID || state.PeerURL != peerURL || !validLocalManifestRoot(state.ManifestRoot) {
		return state, fmt.Errorf("local change state does not match this job")
	}
	return state, nil
}

func loadLocalChangeStateFile(path, owner string) (localChangeState, error) {
	var state localChangeState
	raw, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, err
	}
	if !proto.ValidULID(state.JobID) || !validLocalManifestRoot(state.ManifestRoot) ||
		localChangeKey(state.PeerURL, state.JobID) != owner {
		return state, fmt.Errorf("local change state identity mismatch")
	}
	return state, nil
}

func saveLocalChangeState(state localChangeState) error {
	if !proto.ValidULID(state.JobID) {
		return fmt.Errorf("invalid job ID %q", state.JobID)
	}
	if !validLocalManifestRoot(state.ManifestRoot) {
		return fmt.Errorf("invalid local change manifest root")
	}
	state.PeerURL = strings.TrimSuffix(state.PeerURL, "/")
	if state.Root == "" {
		return fmt.Errorf("local change workspace root is required")
	}
	if state.RootID.IsZero() {
		identity, info, err := fsidentity.Lstat(state.Root)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("local change workspace root is not a directory")
		}
		state.RootID = identity
	}
	root, err := localChangeRoot()
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
	dest := filepath.Join(jobs, localChangeKey(state.PeerURL, state.JobID)+".json")
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

func validLocalManifestRoot(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func discardUnsubmittedChangeState(peerURL, jobID string) error {
	key := localChangeKey(peerURL, jobID)
	unlock, err := acquireLocalChangeLock(localChangeTransferLockName(key))
	if err != nil {
		return err
	}
	defer unlock()
	state, err := loadLocalChangeState(peerURL, jobID)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	remove := func() error {
		current, err := loadLocalChangeState(peerURL, jobID)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.Terminal || current.Pending != "" || len(current.Applied) != 0 {
			return fmt.Errorf("change state is no longer unsubmitted")
		}
		root, err := localChangeRoot()
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
		return withWorkspaceChangeLock(state.Root, remove)
	} else if os.IsNotExist(err) {
		return remove()
	} else {
		return err
	}
}

func markLocalChangeTerminal(peerURL, jobID string) error {
	unlock, err := acquireLocalChangeLock(localChangeTransferLockName(localChangeKey(peerURL, jobID)))
	if err != nil {
		return err
	}
	defer unlock()
	state, err := loadLocalChangeState(peerURL, jobID)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	mark := func() error {
		state, err = loadLocalChangeState(peerURL, jobID)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil || state.Terminal {
			return err
		}
		state.Terminal = true
		return saveLocalChangeState(state)
	}
	if _, err := os.Stat(state.Root); err == nil {
		return withWorkspaceChangeLock(state.Root, mark)
	} else if os.IsNotExist(err) {
		return mark()
	} else {
		return err
	}
}

func markLocalChangeSubmissionStarted(peerURL, jobID string) error {
	unlock, err := acquireLocalChangeLock(localChangeTransferLockName(localChangeKey(peerURL, jobID)))
	if err != nil {
		return err
	}
	defer unlock()
	state, err := loadLocalChangeState(peerURL, jobID)
	if err != nil {
		return err
	}
	mark := func() error {
		state, err = loadLocalChangeState(peerURL, jobID)
		if err != nil || state.SubmissionStarted {
			return err
		}
		state.SubmissionStarted = true
		return saveLocalChangeState(state)
	}
	if _, err := os.Stat(state.Root); err == nil {
		return withWorkspaceChangeLock(state.Root, mark)
	} else if os.IsNotExist(err) {
		return mark()
	} else {
		return err
	}
}

func markLocalChangeAdmissionConfirmed(peerURL, jobID string) error {
	unlock, err := acquireLocalChangeLock(localChangeTransferLockName(localChangeKey(peerURL, jobID)))
	if err != nil {
		return err
	}
	defer unlock()
	state, err := loadLocalChangeState(peerURL, jobID)
	if err != nil {
		return err
	}
	if state.AdmissionConfirmed {
		return nil
	}
	state.AdmissionConfirmed = true
	return saveLocalChangeState(state)
}

func recoverWorkspaceApplications(root string) error {
	return recoverWorkspaceApplicationsContext(context.Background(), root)
}

func recoverWorkspaceApplicationsContext(ctx context.Context, root string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pending, err := changeops.WorkspaceHasApplyTransactions(root)
	if err != nil {
		return err
	}
	if !pending {
		return nil
	}
	stateRoot, err := localChangeRoot()
	if err != nil {
		return err
	}
	jobs := filepath.Join(stateRoot, "jobs")
	entries, err := os.ReadDir(jobs)
	if os.IsNotExist(err) {
		return fmt.Errorf("workspace contains a change apply transaction with no matching local change state")
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
		state, err := loadLocalChangeStateFile(filepath.Join(jobs, entry.Name()), owner)
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
	pending, err = changeops.WorkspaceHasApplyTransactions(root)
	if err != nil {
		return err
	}
	if pending {
		if len(invalidStates) > 0 {
			return invalidStates[0]
		}
		return fmt.Errorf("workspace contains a change apply transaction with no matching local change state")
	}
	return nil
}

func recoverOneApplication(state *localChangeState, owner string) ([]string, error) {
	return recoverOneApplicationContext(context.Background(), state, owner)
}

func recoverOneApplicationContext(ctx context.Context, state *localChangeState, owner string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if state.Pending == "" {
		return nil, nil
	}
	if err := validateLocalWorkspaceIdentity(*state); err != nil {
		return nil, err
	}
	pending, err := changeops.RecoverApplicationToWorkspaceContext(ctx, state.Root, state.Pending, state.RootID)
	if err != nil {
		return nil, err
	}
	if pending == nil {
		state.Pending = ""
		return nil, saveLocalChangeState(*state)
	}
	if pending.Owner != owner {
		return nil, fmt.Errorf("change transaction owner mismatch")
	}
	if state.Applied == nil {
		state.Applied = map[string]string{}
	}
	for _, changePath := range pending.Paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		state.Applied[changePath] = pending.States[changePath]
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := saveLocalChangeState(*state); err != nil {
		return nil, err
	}
	if err := changeops.CommitApplyToWorkspace(state.Root, pending.Transaction, state.RootID); err != nil {
		return nil, err
	}
	state.Pending = ""
	if err := saveLocalChangeState(*state); err != nil {
		return nil, err
	}
	return pending.Paths, nil
}

func withWorkspaceChangeLock(root string, fn func() error) error {
	return withWorkspaceChangeLockContext(context.Background(), root, fn)
}

func withWorkspaceChangeLockContext(ctx context.Context, root string, fn func() error) error {
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
	unlock, err := acquireLocalChangeLockContext(ctx, localChangeWorkspaceLockName(filepath.Clean(abs)))
	if err != nil {
		return err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func validateLocalWorkspaceIdentity(state localChangeState) error {
	identity, info, err := fsidentity.Lstat(state.Root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || identity != state.RootID {
		return fmt.Errorf("local change workspace at %q is not the workspace that submitted job %s", state.Root, state.JobID)
	}
	return nil
}

func acquireLocalChangeLock(name string) (func(), error) {
	return acquireLocalChangeLockContext(context.Background(), name)
}

func openLocalChangeLock(name string) (*os.File, error) {
	root, err := localChangeRoot()
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

func acquireLocalChangeLockContext(ctx context.Context, name string) (func(), error) {
	f, err := openLocalChangeLock(name)
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

func tryAcquireLocalChangeLock(name string) (func(), bool, error) {
	f, err := openLocalChangeLock(name)
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

func tryAcquireExistingLocalChangeLock(name string) (func(), bool, error) {
	root, err := localChangeRoot()
	if err != nil {
		return nil, false, err
	}
	f, err := os.Open(filepath.Join(root, "locks", name+".lock"))
	if os.IsNotExist(err) {
		return func() {}, true, nil
	}
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

func tryAcquireLocalChangeLease(name string) (func(), bool, error) {
	f, err := openLocalChangeLock(name)
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
		// Contenders never wait on a lease, so unlinking before unlock cannot
		// strand a waiter on the old inode. The owner has finished polling before
		// this release runs, making a concurrently recreated lease harmless.
		_ = os.Remove(f.Name())
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true, nil
}

const localChangeLockStripes = 256

func localChangeTransferLockName(key string) string {
	return localChangeStripedLockName("download", key)
}

func localAutomaticApplyLockName(key string) string {
	return localChangeStripedLockName("automatic-apply", key)
}

func localAutomaticApplyWorkerLockName(key string) string {
	return "automatic-apply-worker-" + key
}

func localChangeWorkspaceLockName(root string) string {
	return localChangeStripedLockName("workspace", root)
}

func localChangeStripedLockName(kind, key string) string {
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
	return ensurePrivateLocalDirectoryWithChmod(path, func(dir *os.File, mode os.FileMode) error {
		return dir.Chmod(mode)
	})
}

func ensurePrivateLocalDirectoryWithChmod(
	path string,
	chmod func(*os.File, os.FileMode) error,
) error {
	created := false
	if _, info, err := fsidentity.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("local state directory %q is a symbolic link", path)
		}
		if !info.IsDir() {
			return fmt.Errorf("local state path %q is not a directory", path)
		}
	} else if os.IsNotExist(err) {
		created = true
	} else {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := validateLocalStateAncestors(filepath.Dir(path)); err != nil {
		return err
	}

	pathIdentity, pathInfo, err := fsidentity.Lstat(path)
	if err != nil {
		return err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("local state directory %q is a symbolic link", path)
	}
	if !pathInfo.IsDir() {
		return fmt.Errorf("local state path %q is not a directory", path)
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	dirInfo, err := dir.Stat()
	if err != nil {
		return err
	}
	dirIdentity, err := fsidentity.FromInfo(dirInfo)
	if err != nil {
		return err
	}
	if dirIdentity != pathIdentity {
		return fmt.Errorf("local state directory %q changed while it was being validated", path)
	}
	stat, ok := dirInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("local state directory ownership is unavailable for %q", path)
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("local state directory %q is not owned by the current user", path)
	}

	permissions := dirInfo.Mode().Perm()
	if permissions == 0o700 {
		if currentIdentity, currentInfo, err := fsidentity.Lstat(path); err != nil {
			return err
		} else if currentIdentity != dirIdentity || currentInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("local state directory %q changed while it was being validated", path)
		}
		if created {
			return syncLocalDirectory(filepath.Dir(path))
		}
		return nil
	}
	if err := chmod(dir, 0o700); err != nil {
		if permissions&0o022 != 0 {
			return fmt.Errorf("local state directory %q is writable by other users and permissions could not be corrected: %w", path, err)
		}
		if permissions&0o700 != 0o700 {
			return fmt.Errorf("local state directory %q lacks required owner permissions and could not be corrected: %w", path, err)
		}
		// Extra read or traversal access can reveal entry names, but files remain
		// private and other users cannot replace them. A restricted sandbox may
		// prohibit tightening an otherwise integrity-safe directory.
		return nil
	}
	verifiedInfo, err := dir.Stat()
	if err != nil {
		return err
	}
	verifiedIdentity, err := fsidentity.FromInfo(verifiedInfo)
	if err != nil {
		return err
	}
	if verifiedIdentity != dirIdentity || verifiedInfo.Mode().Perm() != 0o700 {
		return fmt.Errorf("local state directory %q permissions could not be verified", path)
	}
	if currentIdentity, currentInfo, err := fsidentity.Lstat(path); err != nil {
		return err
	} else if currentIdentity != dirIdentity || currentInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("local state directory %q changed while it was being secured", path)
	}
	return syncLocalDirectory(filepath.Dir(path))
}

func validateLocalStateAncestors(parent string) error {
	abs, err := filepath.Abs(parent)
	if err != nil {
		return err
	}
	validateChain := func(start string, followFinalSymlink bool) error {
		for current := filepath.Clean(start); ; current = filepath.Dir(current) {
			var info os.FileInfo
			var statErr error
			if followFinalSymlink {
				info, statErr = os.Stat(current)
			} else {
				info, statErr = os.Lstat(current)
			}
			if statErr != nil {
				return statErr
			}
			if info.Mode()&os.ModeSymlink == 0 && !info.IsDir() {
				return fmt.Errorf("local state ancestor %q is not a directory", current)
			}
			if info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
				return fmt.Errorf("local state ancestor %q is writable by other users", current)
			}
			next := filepath.Dir(current)
			if next == current {
				return nil
			}
		}
	}
	if err := validateChain(abs, false); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return err
	}
	if err := validateChain(resolved, true); err != nil {
		return err
	}
	return nil
}

func localChangeRoot() (string, error) {
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

const localChangeKeyLength = 32 + 1 + 26

func localChangeKey(peerURL, jobID string) string {
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
