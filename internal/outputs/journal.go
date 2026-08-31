package outputs

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
)

const (
	applyJournalVersion         = 1
	applyTransactionPrefix      = ".errand-output-"
	applyJournalFile            = "journal.json"
	applyJournalTempPrefix      = ".journal-"
	applyPhasePrepared          = "prepared"
	applyPhaseCommitted         = "committed"
	applyItemPrepared           = "prepared"
	applyItemInstalling         = "installing"
	applyItemInstalled          = "installed"
	rollbackInstalledName       = "installed"
	applyParentStagingDirectory = "parents"
)

type applyJournal struct {
	Version             int                  `json:"version"`
	Transaction         string               `json:"transaction"`
	TransactionIdentity fsidentity.Identity  `json:"transaction_identity"`
	Owner               string               `json:"owner"`
	BundleRoot          string               `json:"bundle_root"`
	Phase               string               `json:"phase"`
	Items               []applyJournalItem   `json:"items"`
	CreatedParents      []applyJournalParent `json:"created_parents,omitempty"`
}

type applyJournalItem struct {
	Path     string              `json:"path"`
	ItemDir  string              `json:"item_dir"`
	Original Baseline            `json:"original"`
	Expected Baseline            `json:"expected"`
	Parent   fsidentity.Identity `json:"parent_identity,omitempty"`
	Phase    string              `json:"phase"`
}

type applyJournalParent struct {
	Path     string              `json:"path"`
	Identity fsidentity.Identity `json:"identity"`
}

type PendingApply struct {
	Transaction string
	Owner       string
	BundleRoot  string
	Paths       []string
}

func NewApplyTransaction() string {
	return applyTransactionPrefix + proto.NewULID()
}

func WorkspaceHasApplyTransactions(root string) (bool, error) {
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return false, err
	}
	defer rootFS.Close()
	dir, err := rootFS.Open(".")
	if err != nil {
		return false, err
	}
	for {
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			if entry.IsDir() && validApplyTransaction(entry.Name()) {
				return true, dir.Close()
			}
		}
		if errors.Is(readErr, io.EOF) {
			return false, dir.Close()
		}
		if readErr != nil {
			return false, errors.Join(readErr, dir.Close())
		}
	}
}

func WorkspaceContainsApplyTransaction(root, transaction string, expectedRoot fsidentity.Identity) (bool, error) {
	if !validApplyTransaction(transaction) {
		return false, fmt.Errorf("invalid output transaction name %q", transaction)
	}
	destination, err := openApplyDestinationWithIdentity(root, expectedRoot)
	if err != nil {
		return false, err
	}
	defer destination.Close()
	info, err := destination.root.Lstat(transaction)
	if os.IsNotExist(err) {
		return false, destination.verifyPath()
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("output transaction %s is not a directory", transaction)
	}
	if err := destination.verifyPath(); err != nil {
		return false, err
	}
	return true, nil
}

// RecoverApplication accepts only a transaction ID already registered in
// Errand's private local state. It rolls back an interrupted installation or
// returns a committed installation whose state record must be advanced.
func RecoverApplication(root, transaction string) (*PendingApply, error) {
	return RecoverApplicationContext(context.Background(), root, transaction)
}

func RecoverApplicationContext(ctx context.Context, root, transaction string) (*PendingApply, error) {
	identity, err := applyWorkspaceIdentity(root)
	if err != nil {
		return nil, err
	}
	return RecoverApplicationToWorkspaceContext(ctx, root, transaction, identity)
}

// RecoverApplicationToWorkspaceContext recovers only within the workspace
// identified when the job was submitted.
func RecoverApplicationToWorkspaceContext(ctx context.Context, root, transaction string, expectedRoot fsidentity.Identity) (*PendingApply, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validApplyTransaction(transaction) {
		return nil, fmt.Errorf("invalid output transaction name %q", transaction)
	}
	destination, err := openApplyDestinationWithIdentity(root, expectedRoot)
	if err != nil {
		return nil, err
	}
	defer destination.Close()
	pending, recoverErr := recoverApplicationAtRootContext(ctx, destination.root, transaction)
	if pathErr := destination.verifyPath(); pathErr != nil {
		return pending, errors.Join(recoverErr, pathErr)
	}
	return pending, recoverErr
}

func recoverApplicationAtRootContext(ctx context.Context, root *os.Root, transaction string) (*PendingApply, error) {
	journal, err := loadApplyJournalAtRoot(root, transaction)
	if os.IsNotExist(err) {
		return nil, removeUnpublishedApplyTransactionAtRoot(ctx, root, transaction)
	}
	if err != nil {
		return nil, fmt.Errorf("recovering output transaction %s: %w", transaction, err)
	}
	if journal.Phase == applyPhaseCommitted {
		paths := make([]string, len(journal.Items))
		for i, item := range journal.Items {
			current, err := captureBaselineAtRootContext(ctx, root, item.Path, item.Path)
			if err != nil {
				return nil, err
			}
			if !sameBaselineContent(current, item.Expected) {
				return nil, fmt.Errorf("committed output %s changed before state recovery; original retained in %s",
					item.Path, path.Join(journal.Transaction, item.ItemDir, "previous"))
			}
			paths[i] = item.Path
		}
		return &PendingApply{
			Transaction: journal.Transaction, Owner: journal.Owner,
			BundleRoot: journal.BundleRoot, Paths: paths,
		}, nil
	}
	if err := rollbackApplyJournalAtRootContext(ctx, root, journal); err != nil {
		return nil, fmt.Errorf("recovering output transaction %s: %w", transaction, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := removeApplyTransactionAtRoot(root, journal.Transaction, journal.TransactionIdentity); err != nil {
		return nil, err
	}
	return nil, nil
}

func CommitApply(root, transaction string) error {
	identity, err := applyWorkspaceIdentity(root)
	if err != nil {
		return err
	}
	return CommitApplyToWorkspace(root, transaction, identity)
}

func OutputPathMatches(root string, bundle proto.OutputBundle, outputPath string) (bool, error) {
	identity, err := applyWorkspaceIdentity(root)
	if err != nil {
		return false, err
	}
	return OutputPathMatchesWorkspace(root, identity, bundle, outputPath)
}

func CommitApplyToWorkspace(root, transaction string, expectedRoot fsidentity.Identity) error {
	destination, err := openApplyDestinationWithIdentity(root, expectedRoot)
	if err != nil {
		return err
	}
	defer destination.Close()
	journal, err := loadApplyJournalAtRoot(destination.root, transaction)
	if err != nil {
		return errors.Join(err, destination.verifyPath())
	}
	removeErr := removeApplyTransactionAtRoot(destination.root, transaction, journal.TransactionIdentity)
	return errors.Join(removeErr, destination.verifyPath())
}

func OutputPathMatchesWorkspace(root string, expectedRoot fsidentity.Identity, bundle proto.OutputBundle, outputPath string) (bool, error) {
	destination, err := openApplyDestinationWithIdentity(root, expectedRoot)
	if err != nil {
		return false, err
	}
	defer destination.Close()
	got, captureErr := captureBaselineAtRootContext(context.Background(), destination.root, outputPath, outputPath)
	if pathErr := destination.verifyPath(); pathErr != nil {
		return false, errors.Join(captureErr, pathErr)
	}
	if captureErr != nil {
		return false, captureErr
	}
	want := subtreeManifest(bundle.Manifest, outputPath).RootHash()
	return !got.Missing && got.Digest == want, nil
}

func writeApplyJournal(root string, journal applyJournal) error {
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer rootFS.Close()
	return writeApplyJournalAtRoot(rootFS, journal)
}

func writeApplyJournalAtRoot(root *os.Root, journal applyJournal) error {
	if err := validateApplyJournal(journal); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	if len(raw)+1 > MaxBundleMetadataBytes {
		return fmt.Errorf("output apply journal exceeds %d bytes", MaxBundleMetadataBytes)
	}
	tmpName := path.Join(journal.Transaction, applyJournalTempPrefix+proto.NewULID())
	tmp, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
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
	if err := root.Rename(tmpName, path.Join(journal.Transaction, applyJournalFile)); err != nil {
		return err
	}
	return syncApplyRootDirectory(root, journal.Transaction)
}

func removeUnpublishedApplyTransactionAtRoot(ctx context.Context, root *os.Root, transaction string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := root.Lstat(transaction)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	identity, err := fsidentity.FromInfo(info)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("output transaction %s is not a directory", transaction)
	}
	dir, err := root.Open(transaction)
	if err != nil {
		return err
	}
	entries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validApplyJournalTemp(entry.Name()) {
			return fmt.Errorf("output transaction %s has no journal and contains unexpected recovery data; retained", transaction)
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if !entryInfo.Mode().IsRegular() {
			return fmt.Errorf("output transaction %s has a non-regular journal temporary; retained", transaction)
		}
	}
	if err := verifyApplyTransactionIdentityAtRoot(root, transaction, identity); err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := root.Remove(path.Join(transaction, entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := syncApplyRootDirectory(root, transaction); err != nil {
		return err
	}
	if err := verifyApplyTransactionIdentityAtRoot(root, transaction, identity); err != nil {
		return err
	}
	if err := root.Remove(transaction); err != nil {
		return err
	}
	return syncApplyRootDirectory(root, ".")
}

func validApplyJournalTemp(name string) bool {
	return strings.HasPrefix(name, applyJournalTempPrefix) &&
		proto.ValidULID(strings.TrimPrefix(name, applyJournalTempPrefix))
}

func loadApplyJournal(root, transaction string) (applyJournal, error) {
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return applyJournal{}, err
	}
	defer rootFS.Close()
	return loadApplyJournalAtRoot(rootFS, transaction)
}

func loadApplyJournalAtRoot(root *os.Root, transaction string) (applyJournal, error) {
	var journal applyJournal
	if !validApplyTransaction(transaction) {
		return journal, fmt.Errorf("invalid output transaction name %q", transaction)
	}
	f, err := root.Open(path.Join(transaction, applyJournalFile))
	if err != nil {
		return journal, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxBundleMetadataBytes+1))
	if err != nil {
		return journal, err
	}
	if len(raw) > MaxBundleMetadataBytes {
		return journal, fmt.Errorf("output apply journal exceeds %d bytes", MaxBundleMetadataBytes)
	}
	if err := json.Unmarshal(raw, &journal); err != nil {
		return journal, err
	}
	if journal.Transaction != transaction {
		return journal, fmt.Errorf("output apply journal names transaction %q", journal.Transaction)
	}
	if err := validateApplyJournal(journal); err != nil {
		return journal, err
	}
	if err := verifyApplyTransactionIdentityAtRoot(root, transaction, journal.TransactionIdentity); err != nil {
		return journal, err
	}
	return journal, nil
}

func validateApplyJournal(journal applyJournal) error {
	if journal.Version != applyJournalVersion || !validApplyTransaction(journal.Transaction) ||
		journal.TransactionIdentity.IsZero() || journal.Owner == "" || len(journal.BundleRoot) != 64 || len(journal.Items) == 0 {
		return fmt.Errorf("invalid output apply journal")
	}
	if _, err := hex.DecodeString(journal.BundleRoot); err != nil {
		return fmt.Errorf("invalid output bundle root")
	}
	if journal.Phase != applyPhasePrepared && journal.Phase != applyPhaseCommitted {
		return fmt.Errorf("invalid output apply phase %q", journal.Phase)
	}
	seen := map[string]bool{}
	for i, item := range journal.Items {
		if err := validatePath(item.Path); err != nil {
			return err
		}
		wantDir := fmt.Sprintf("%06d", i)
		if item.ItemDir != wantDir || seen[item.Path] || item.Original.Path != item.Path || item.Expected.Path != item.Path || item.Expected.Missing {
			return fmt.Errorf("invalid output apply journal item %q", item.Path)
		}
		if item.Phase != applyItemPrepared && item.Phase != applyItemInstalling && item.Phase != applyItemInstalled {
			return fmt.Errorf("invalid output apply item phase %q", item.Phase)
		}
		if item.Phase != applyItemPrepared && item.Parent.IsZero() {
			return fmt.Errorf("output apply item %q is missing its parent identity", item.Path)
		}
		seen[item.Path] = true
	}
	seenParents := map[string]bool{}
	for _, parent := range journal.CreatedParents {
		if err := validatePath(parent.Path); err != nil {
			return err
		}
		if parent.Identity.IsZero() {
			return fmt.Errorf("created output parent %q is missing its identity", parent.Path)
		}
		if seenParents[parent.Path] {
			return fmt.Errorf("duplicate created output parent %q", parent.Path)
		}
		seenParents[parent.Path] = true
	}
	return nil
}

func rollbackApplyJournal(root string, journal applyJournal) error {
	return rollbackApplyJournalContext(context.Background(), root, journal)
}

func rollbackApplyJournalContext(ctx context.Context, root string, journal applyJournal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer rootFS.Close()
	return rollbackApplyJournalAtRootContext(ctx, rootFS, journal)
}

func rollbackApplyJournalAtRootContext(ctx context.Context, rootFS *os.Root, journal applyJournal) error {
	var rollbackErr error
	for i := len(journal.Items) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return errors.Join(rollbackErr, err)
		}
		item := journal.Items[i]
		if item.Phase == applyItemPrepared {
			continue
		}
		destinationDir, err := openOutputParent(rootFS, item.Path, item.Parent)
		if err != nil {
			rollbackErr = errors.Join(rollbackErr,
				fmt.Errorf("output parent for %s changed; recovery data retained: %w", item.Path, err))
			continue
		}
		itemErr := rollbackApplyItemAtRootContext(ctx, rootFS, journal, item, destinationDir)
		parentErr := verifyOutputParent(rootFS, item.Path, item.Parent)
		closeErr := destinationDir.Close()
		if parentErr != nil {
			parentErr = fmt.Errorf("output parent for %s changed; recovery data retained: %w", item.Path, parentErr)
		}
		rollbackErr = errors.Join(rollbackErr, itemErr, parentErr, closeErr)
	}
	if rollbackErr != nil {
		return rollbackErr
	}
	for i := len(journal.CreatedParents) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return errors.Join(rollbackErr, err)
		}
		parent := journal.CreatedParents[i]
		staged := path.Join(journal.Transaction, applyParentStagingDirectory, fmt.Sprintf("%06d", i))
		if _, err := rootFS.Lstat(staged); err == nil {
			// The no-replace rename never installed this parent. Another process
			// may have created the destination after preflight, so preserve it.
			continue
		} else if !os.IsNotExist(err) {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("inspecting staged output parent %s: %w", parent.Path, err))
			continue
		}
		info, err := rootFS.Lstat(parent.Path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("inspecting created output parent %s: %w", parent.Path, err))
			continue
		}
		identity, err := fsidentity.FromInfo(info)
		if err != nil || identity != parent.Identity {
			rollbackErr = errors.Join(rollbackErr,
				fmt.Errorf("created output parent %s changed; recovery data retained", parent.Path))
			continue
		}
		if err := rootFS.Remove(parent.Path); err != nil && !os.IsNotExist(err) {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("removing created output parent %s: %w", parent.Path, err))
			continue
		}
		if err := syncApplyPathAtRoot(rootFS, parent.Path); err != nil && !os.IsNotExist(err) {
			rollbackErr = errors.Join(rollbackErr, err)
		}
	}
	return rollbackErr
}

func rollbackApplyItemAtRootContext(
	ctx context.Context,
	rootFS *os.Root,
	journal applyJournal,
	item applyJournalItem,
	destinationDir *os.File,
) error {
	backup := path.Join(journal.Transaction, item.ItemDir, "previous")
	quarantine := path.Join(journal.Transaction, item.ItemDir, rollbackInstalledName)
	_, backupErr := rootFS.Lstat(backup)
	hasBackup := backupErr == nil
	if backupErr != nil && !os.IsNotExist(backupErr) {
		return fmt.Errorf("inspecting backup for %s: %w", item.Path, backupErr)
	}
	if hasBackup {
		hasQuarantine, err := quarantineInstalledOutput(rootFS, item.Path, quarantine, destinationDir)
		if err != nil {
			return fmt.Errorf("quarantining installed %s: %w", item.Path, err)
		}
		if hasQuarantine {
			installed, err := captureBaselineAtRootContext(ctx, rootFS, quarantine, item.Path)
			if err != nil {
				return fmt.Errorf("inspecting installed %s: %w", item.Path, err)
			}
			if !sameBaselineContent(installed, item.Expected) {
				restoreErr := renameApplyPathToDirectoryNoReplace(rootFS, quarantine, destinationDir, path.Base(item.Path))
				return errors.Join(
					fmt.Errorf("output %s changed after installation; original retained in %s", item.Path, backup),
					restoreErr)
			}
		}
		if err := renameApplyPathToDirectoryNoReplace(rootFS, backup, destinationDir, path.Base(item.Path)); err != nil {
			return fmt.Errorf("restoring %s: %w", item.Path, err)
		}
		if hasQuarantine {
			if err := rootFS.RemoveAll(quarantine); err != nil {
				return fmt.Errorf("removing quarantined output %s: %w", item.Path, err)
			}
		}
		return nil
	}
	if item.Original.Missing {
		hasQuarantine, err := quarantineInstalledOutput(rootFS, item.Path, quarantine, destinationDir)
		if err != nil {
			return fmt.Errorf("quarantining new output %s: %w", item.Path, err)
		}
		if !hasQuarantine {
			return nil
		}
		installed, err := captureBaselineAtRootContext(ctx, rootFS, quarantine, item.Path)
		if err != nil {
			return fmt.Errorf("inspecting new output %s: %w", item.Path, err)
		}
		if !sameBaselineContent(installed, item.Expected) {
			restoreErr := renameApplyPathToDirectoryNoReplace(rootFS, quarantine, destinationDir, path.Base(item.Path))
			return errors.Join(fmt.Errorf("new output %s changed after installation", item.Path), restoreErr)
		}
		if err := rootFS.RemoveAll(quarantine); err != nil {
			return fmt.Errorf("removing new output %s: %w", item.Path, err)
		}
		return nil
	}
	current, err := captureBaselineAtRootContext(ctx, rootFS, item.Path, item.Path)
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", item.Path, err)
	}
	if !sameBaseline(item.Original, current) {
		return fmt.Errorf("original output %s and its backup are unavailable", item.Path)
	}
	return nil
}

func quarantineInstalledOutput(root *os.Root, outputPath, quarantine string, outputParent *os.File) (bool, error) {
	if _, err := root.Lstat(quarantine); err == nil {
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	quarantineDir, err := root.Open(path.Dir(quarantine))
	if err != nil {
		return false, err
	}
	defer quarantineDir.Close()
	if err := renameNoReplace(outputParent, path.Base(outputPath), quarantineDir, path.Base(quarantine)); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		if errors.Is(err, fs.ErrExist) {
			return true, nil
		}
		return false, err
	}
	return true, errors.Join(outputParent.Sync(), quarantineDir.Sync())
}

func renameApplyPathToDirectoryNoReplace(root *os.Root, from string, toDir *os.File, toName string) error {
	fromDir, err := root.Open(path.Dir(from))
	if err != nil {
		return err
	}
	defer fromDir.Close()
	if err := renameNoReplace(fromDir, path.Base(from), toDir, toName); err != nil {
		return err
	}
	return errors.Join(fromDir.Sync(), toDir.Sync())
}

func renameApplyPathNoReplace(root *os.Root, from, to string) error {
	fromDir, err := root.Open(path.Dir(from))
	if err != nil {
		return err
	}
	defer fromDir.Close()
	toDir, err := root.Open(path.Dir(to))
	if err != nil {
		return err
	}
	defer toDir.Close()
	if err := renameNoReplace(fromDir, path.Base(from), toDir, path.Base(to)); err != nil {
		return err
	}
	return errors.Join(fromDir.Sync(), toDir.Sync())
}

func removeApplyTransaction(root string, journal applyJournal) error {
	if !validApplyTransaction(journal.Transaction) {
		return fmt.Errorf("invalid output transaction name %q", journal.Transaction)
	}
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer rootFS.Close()
	return removeApplyTransactionAtRoot(rootFS, journal.Transaction, journal.TransactionIdentity)
}

func removeApplyTransactionAtRoot(rootFS *os.Root, transaction string, expected fsidentity.Identity) error {
	if !validApplyTransaction(transaction) {
		return fmt.Errorf("invalid output transaction name %q", transaction)
	}
	if err := verifyApplyTransactionIdentityAtRoot(rootFS, transaction, expected); err != nil {
		return err
	}
	if err := rootFS.RemoveAll(transaction); err != nil {
		return err
	}
	return syncApplyRootDirectory(rootFS, ".")
}

func verifyApplyTransactionIdentityAtRoot(root *os.Root, transaction string, expected fsidentity.Identity) error {
	info, err := root.Lstat(transaction)
	if err != nil {
		return err
	}
	identity, err := fsidentity.FromInfo(info)
	if err != nil || !info.IsDir() || identity != expected {
		return fmt.Errorf("output transaction %s changed; recovery data retained", transaction)
	}
	return nil
}

func validApplyTransaction(name string) bool {
	if !strings.HasPrefix(name, applyTransactionPrefix) {
		return false
	}
	return proto.ValidULID(strings.TrimPrefix(name, applyTransactionPrefix))
}

func syncApplyPath(root, outputPath string) error {
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer rootFS.Close()
	return syncApplyPathAtRoot(rootFS, outputPath)
}

func syncApplyPathAtRoot(root *os.Root, outputPath string) error {
	return syncApplyRootDirectory(root, path.Dir(filepath.ToSlash(outputPath)))
}

func syncApplyRootDirectory(root *os.Root, directory string) error {
	dir, err := root.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
