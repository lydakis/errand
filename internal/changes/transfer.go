package changes

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
	"golang.org/x/sys/unix"
)

// TransferTarget records one immutable apply request in private receiver state.
// StatePath's parent must exist outside Root and be owned by the caller. The
// caller serializes applications and recovery for this destination, retains the
// staged bundle, and reuses StatePath only for retries of the SAME request.
// A deliberate new application, even of the same bundle, needs a new StatePath.
// This does not lock user jobs or advance a synchronization baseline.
type TransferTarget struct {
	Root      string
	RootID    fsidentity.Identity
	Owner     string
	StatePath string
}

type transferApplyState struct {
	Version     int                       `json:"version"`
	Owner       string                    `json:"owner"`
	RootID      fsidentity.Identity       `json:"root_identity"`
	BundleRoot  string                    `json:"bundle_root"`
	Paths       []string                  `json:"paths"`
	Materialize bool                      `json:"materialize_conflicts"`
	Pending     string                    `json:"pending,omitempty"`
	CleanupID   *fsidentity.Identity      `json:"cleanup_identity,omitempty"`
	Outcome     *transferOutcome          `json:"outcome,omitempty"`
	Checkpoint  *checkpointReceiptBinding `json:"checkpoint,omitempty"`
}

type transferOutcome struct {
	Applied   []string          `json:"applied,omitempty"`
	Conflicts []string          `json:"conflicts,omitempty"`
	States    map[string]string `json:"states,omitempty"`
	Refused   bool              `json:"refused,omitempty"`
}

// Apply merges once, durably records its outcome, and then removes the apply
// journal. Retrying a completed request replays that historical outcome without
// inspecting or overwriting subsequent file edits. Conflict reports are outcomes,
// not a resolution index. States describe installed destination values, including
// markers and preserved binary values; they do NOT mean all source edits landed.
func (t TransferTarget) Apply(staged string, bundle proto.ChangeBundle, selected map[string]bool, options ApplyOptions) (ApplyResult, error) {
	if err := validateBundle(bundle); err != nil {
		return ApplyResult{}, err
	}
	if t.Owner == "" || t.RootID.IsZero() || !filepath.IsAbs(t.StatePath) {
		return ApplyResult{}, fmt.Errorf("transfer application requires an owner, workspace identity, and absolute state path")
	}
	destination, err := openApplyDestinationWithIdentity(t.Root, t.RootID)
	if err != nil {
		return ApplyResult{}, err
	}
	defer destination.Close()
	parent, err := filepath.EvalSymlinks(filepath.Dir(t.StatePath))
	if err != nil {
		return ApplyResult{}, err
	}
	storage, err := openApplyDestination(parent)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("opening transfer state directory: %w", err)
	}
	defer storage.Close()
	if err := transferStorageOutsideWorkspace(storage.root, t.RootID); err != nil {
		return ApplyResult{}, err
	}
	name := filepath.Base(t.StatePath)
	request := transferApplyState{Version: 1, Owner: t.Owner, RootID: t.RootID, BundleRoot: bundle.RootHash(), Materialize: options.MaterializeConflicts}
	for path, enabled := range selected {
		_, exists := slices.BinarySearch(bundle.Paths, path)
		if enabled && !exists {
			return ApplyResult{}, fmt.Errorf("selected transfer path %q is not a change root", path)
		}
	}
	for _, path := range bundle.Paths {
		if selected == nil || selected[path] {
			request.Paths = append(request.Paths, path)
		}
	}
	state, err := readTransferState(storage.root, name)
	if errors.Is(err, os.ErrNotExist) {
		state = request
	} else if err != nil {
		return ApplyResult{}, err
	} else if state.Version != request.Version || state.Owner != request.Owner || state.RootID != request.RootID ||
		state.BundleRoot != request.BundleRoot || state.Materialize != request.Materialize || !slices.Equal(state.Paths, request.Paths) {
		return ApplyResult{}, fmt.Errorf("transfer application does not match its recorded request")
	}
	if err := state.validateOutcome(bundle); err != nil {
		return ApplyResult{}, err
	}
	save := func() error {
		if err := state.validateOutcome(bundle); err != nil {
			return err
		}
		if err := storage.verifyPath(); err != nil {
			return err
		}
		return writeTransferState(storage.root, name, state)
	}
	if err := t.recoverTransfer(destination, &state, save); err != nil {
		return ApplyResult{}, err
	}
	if state.Outcome == nil {
		state.Pending = NewApplyTransaction()
		if err := save(); err != nil {
			return ApplyResult{}, err
		}
		result, applyErr := ApplyToWorkspace(staged, t.Root, bundle, selected, t.Owner, state.Pending, t.RootID, options)
		if applyErr != nil {
			// Roll back an interrupted installation before recording any refusal.
			if err := t.recoverTransfer(destination, &state, save); err != nil {
				return ApplyResult{}, errors.Join(applyErr, err)
			}
			if state.Outcome == nil {
				if err := state.recordRefusal(applyErr); err != nil {
					return ApplyResult{}, err
				}
			}
		} else {
			state.Outcome = &transferOutcome{Applied: result.Applied, Conflicts: result.Conflicts, States: result.States}
		}
		// This write precedes journal deletion. On interruption the journal can
		// reconstruct the outcome, including materialized and preserved conflicts.
		if err := save(); err != nil {
			return ApplyResult{}, err
		}
		if err := t.finishTransfer(destination, &state, save); err != nil {
			return ApplyResult{}, err
		}
	}
	if err := destination.verifyPath(); err != nil {
		return ApplyResult{}, err
	}
	result := ApplyResult{Applied: append([]string(nil), state.Outcome.Applied...), Conflicts: append([]string(nil), state.Outcome.Conflicts...), States: state.Outcome.States, BundleRoot: state.BundleRoot}
	if len(result.Conflicts) != 0 {
		return result, &MergeConflictError{Paths: result.Conflicts, Materialized: !state.Outcome.Refused}
	}
	return result, nil
}

func (t TransferTarget) recoverTransfer(destination *applyDestination, state *transferApplyState, save func() error) error {
	if state.Pending == "" {
		return nil
	}
	if state.Outcome != nil {
		return t.finishTransfer(destination, state, save)
	}
	// Check identity before recovery, which may roll back an unfinished journal.
	_, err := loadTransferJournal(destination.root, *state)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	pending, err := RecoverApplicationToWorkspaceContext(context.Background(), t.Root, state.Pending, t.RootID)
	if err != nil {
		return err
	}
	if pending != nil {
		state.Outcome = &transferOutcome{
			Applied:   rootsOutsideConflicts(pending.Paths, pending.Conflicts),
			Conflicts: pending.Conflicts, States: pending.States,
		}
		if err := save(); err != nil {
			return err
		}
		return t.finishTransfer(destination, state, save)
	}
	state.Pending = ""
	return save()
}

// finishTransfer runs only after the outcome is durable. Installed files may
// already contain later edits. Validate the journal and backups once, then durably
// authorize cleanup of that transaction identity before deleting any data. Retries
// can finish partial deletion without requiring the deleted journal or backups.
// Existing job-apply callers retain their stricter commit semantics.
func (t TransferTarget) finishTransfer(destination *applyDestination, state *transferApplyState, save func() error) error {
	if state.Pending == "" {
		return nil
	}
	if state.Outcome == nil {
		return fmt.Errorf("transfer cleanup requires a durable outcome")
	}
	if state.CleanupID == nil {
		journal, err := loadTransferJournal(destination.root, *state)
		if errors.Is(err, os.ErrNotExist) {
			// No installation journal (for example, an empty selection). Do not
			// authorize deletion of unknown recovery data without a checkpoint.
			if _, err := RecoverApplicationToWorkspaceContext(context.Background(), t.Root, state.Pending, t.RootID); err != nil {
				return err
			}
			state.Pending = ""
			return save()
		}
		if err != nil {
			return err
		}
		if journal.Phase != applyPhaseCommitted {
			return fmt.Errorf("completed transfer has an uncommitted apply journal")
		}
		if err := validateApplyBackups(destination.root, journal); err != nil {
			return fmt.Errorf("validating transfer backups before cleanup: %w", err)
		}
		state.CleanupID = &journal.TransactionIdentity
		if err := save(); err != nil {
			return err
		}
	}
	if err := destination.verifyPath(); err != nil {
		return err
	}
	err := verifyApplyTransactionIdentityAtRoot(destination.root, state.Pending, *state.CleanupID)
	if errors.Is(err, os.ErrNotExist) {
		// Deletion finished before the receipt could be cleared. Make that
		// absence durable before forgetting the pending transaction.
		err = syncApplyRootDirectory(destination.root, ".")
	} else if err == nil {
		err = removeApplyTransactionAtRoot(destination.root, state.Pending, *state.CleanupID)
	}
	if err != nil {
		return err
	}
	if err := destination.verifyPath(); err != nil {
		return err
	}
	state.Pending = ""
	state.CleanupID = nil
	return save()
}

func loadTransferJournal(root *os.Root, state transferApplyState) (applyJournal, error) {
	if !validApplyTransaction(state.Pending) {
		return applyJournal{}, fmt.Errorf("invalid pending transfer transaction")
	}
	journal, err := loadApplyJournalAtRoot(root, state.Pending)
	if err == nil && (journal.Owner != state.Owner || journal.BundleRoot != state.BundleRoot) {
		return applyJournal{}, fmt.Errorf("pending journal does not belong to this transfer application")
	}
	return journal, err
}

func (s *transferApplyState) recordRefusal(applyErr error) error {
	var conflict *MergeConflictError
	// A conflict escaping materialization means the attempt failed, often because
	// the destination changed during installation. It is not a completed refusal.
	if s.Materialize || !errors.As(applyErr, &conflict) || conflict.Materialized {
		return applyErr
	}
	s.Outcome = &transferOutcome{Conflicts: mergeConflictPaths(conflict.Paths), Refused: true}
	return nil
}

// Walk opened parent directories, so case aliases and symlinks cannot make
// receiver state inside the workspace appear to be elsewhere.
func transferStorageOutsideWorkspace(storage *os.Root, workspace fsidentity.Identity) error {
	current, err := storage.Open(".")
	if err != nil {
		return err
	}
	defer func() { current.Close() }()
	for {
		info, err := current.Stat()
		if err != nil {
			return err
		}
		id, err := fsidentity.FromInfo(info)
		if err != nil {
			return err
		}
		if id == workspace {
			return fmt.Errorf("transfer state must be outside the destination workspace")
		}
		fd, err := unix.Openat(int(current.Fd()), "..", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		parent := os.NewFile(uintptr(fd), "transfer state ancestor")
		parentInfo, err := parent.Stat()
		if err != nil {
			parent.Close()
			return err
		}
		parentID, err := fsidentity.FromInfo(parentInfo)
		if err != nil {
			parent.Close()
			return err
		}
		if parentID == id {
			return parent.Close()
		}
		current.Close()
		current = parent
	}
}

func (s transferApplyState) validateOutcome(bundle proto.ChangeBundle) error {
	if s.Pending != "" && !validApplyTransaction(s.Pending) {
		return fmt.Errorf("invalid pending transfer transaction")
	}
	if s.CleanupID != nil && (s.CleanupID.IsZero() || s.Pending == "" || s.Outcome == nil || s.Outcome.Refused) {
		return fmt.Errorf("invalid transfer cleanup checkpoint")
	}
	if s.Outcome == nil {
		return nil
	}
	o := s.Outcome
	invalid := func() error { return fmt.Errorf("invalid recorded transfer outcome") }
	if o.Refused && (s.Materialize || len(o.Conflicts) == 0 || len(o.Applied) != 0 || len(o.States) != 0) {
		return invalid()
	}
	if !o.Refused && !s.Materialize && len(o.Conflicts) != 0 {
		return invalid()
	}
	for i, conflict := range o.Conflicts {
		if validatePath(conflict) != nil || (i > 0 && o.Conflicts[i-1] >= conflict) ||
			len(rootsOutsideConflicts(s.Paths, []string{conflict})) == len(s.Paths) {
			return invalid()
		}
	}
	for path, state := range o.States {
		if _, found := slices.BinarySearch(s.Paths, path); !found {
			return invalid()
		}
		if strings.HasPrefix(state, metadataStatePrefix) != bundleHasMetadataPath(bundle, path) {
			return invalid()
		}
		digest := strings.TrimPrefix(state, metadataStatePrefix)
		if digest != "missing" {
			if _, err := hex.DecodeString(digest); err != nil || len(digest) != 64 {
				return invalid()
			}
		}
	}
	seen := map[string]bool{}
	for _, path := range o.Applied {
		if seen[path] || o.States[path] == "" || len(rootsOutsideConflicts([]string{path}, o.Conflicts)) == 0 {
			return invalid()
		}
		seen[path] = true
	}
	if !o.Refused {
		for _, path := range rootsOutsideConflicts(s.Paths, o.Conflicts) {
			if !seen[path] {
				return invalid()
			}
		}
	}
	return nil
}

func readTransferState(root *os.Root, name string) (transferApplyState, error) {
	var state transferApplyState
	err := readTransferRecord(root, name, &state)
	return state, err
}

func readTransferRecord(root *os.Root, name string, record any) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("transfer state is not a regular file")
	}
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxBundleMetadataBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > MaxBundleMetadataBytes {
		return fmt.Errorf("transfer state exceeds size limit")
	}
	return json.Unmarshal(raw, record)
}

func writeTransferState(root *os.Root, name string, state transferApplyState) error {
	return writeTransferRecord(root, name, state)
}

func verifyTransferPaths(paths ...*applyDestination) error {
	for _, p := range paths {
		if err := p.verifyPath(); err != nil {
			return err
		}
	}
	return nil
}

func writeVerifiedTransferRecord(destination, storage *applyDestination, name string, record any) error {
	if err := verifyTransferPaths(destination, storage); err != nil {
		return err
	}
	if err := writeTransferRecord(storage.root, name, record); err != nil {
		return err
	}
	return verifyTransferPaths(destination, storage)
}

func writeTransferRecord(root *os.Root, name string, record any) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(raw) > MaxBundleMetadataBytes {
		return fmt.Errorf("transfer state exceeds size limit")
	}
	tmpName := ".transfer-" + proto.NewULID()
	f, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmpName)
	_, writeErr := f.Write(raw)
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := root.Rename(tmpName, name); err != nil {
		return err
	}
	return syncApplyRootDirectory(root, ".")
}
