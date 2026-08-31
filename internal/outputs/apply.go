package outputs

import (
	"context"
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

type applyDestination struct {
	path     string
	root     *os.Root
	identity fsidentity.Identity
}

func openApplyDestination(destinationRoot string) (*applyDestination, error) {
	identity, err := applyWorkspaceIdentity(destinationRoot)
	if err != nil {
		return nil, err
	}
	return openApplyDestinationWithIdentity(destinationRoot, identity)
}

func openApplyDestinationWithIdentity(destinationRoot string, expected fsidentity.Identity) (*applyDestination, error) {
	identity, info, err := fsidentity.Lstat(destinationRoot)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("local output workspace root is not a directory")
	}
	if identity != expected {
		return nil, fmt.Errorf("local output workspace at %q is not the workspace that submitted the job", destinationRoot)
	}
	root, err := os.OpenRoot(destinationRoot)
	if err != nil {
		return nil, err
	}
	opened, err := root.Lstat(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	openedIdentity, err := fsidentity.FromInfo(opened)
	if err != nil || openedIdentity != identity {
		root.Close()
		return nil, fmt.Errorf("local output workspace root changed while application started")
	}
	return &applyDestination{path: destinationRoot, root: root, identity: identity}, nil
}

func applyWorkspaceIdentity(destinationRoot string) (fsidentity.Identity, error) {
	identity, info, err := fsidentity.Lstat(destinationRoot)
	if err != nil {
		return fsidentity.Identity{}, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return fsidentity.Identity{}, fmt.Errorf("local output workspace root is not a directory")
	}
	return identity, nil
}

func (d *applyDestination) Close() error {
	return d.root.Close()
}

func (d *applyDestination) verifyPath() error {
	identity, info, err := fsidentity.Lstat(d.path)
	if err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || identity != d.identity {
		return fmt.Errorf("local output workspace root changed during application")
	}
	return nil
}

// Apply prepares and installs one set of outputs, leaving a durable transaction
// journal until the caller records its local apply state and calls CommitApply.
func Apply(
	stagedRoot string,
	destinationRoot string,
	bundle proto.OutputBundle,
	baselines []Baseline,
	selected map[string]bool,
	owner string,
	transaction string,
) (ApplyResult, error) {
	identity, err := applyWorkspaceIdentity(destinationRoot)
	if err != nil {
		return ApplyResult{}, err
	}
	return ApplyToWorkspace(
		stagedRoot, destinationRoot, bundle, baselines, selected, owner, transaction, identity,
	)
}

// ApplyToWorkspace applies outputs only if destinationRoot still identifies
// the workspace recorded when the job was submitted.
func ApplyToWorkspace(
	stagedRoot string,
	destinationRoot string,
	bundle proto.OutputBundle,
	baselines []Baseline,
	selected map[string]bool,
	owner string,
	transaction string,
	expectedRoot fsidentity.Identity,
) (ApplyResult, error) {
	if err := validateBundle(bundle); err != nil {
		return ApplyResult{}, err
	}
	if owner == "" {
		return ApplyResult{}, fmt.Errorf("output apply owner is required")
	}
	if !validApplyTransaction(transaction) {
		return ApplyResult{}, fmt.Errorf("invalid output transaction name %q", transaction)
	}
	baselineByPath := make(map[string]Baseline, len(baselines))
	for _, baseline := range baselines {
		baselineByPath[baseline.Path] = baseline
	}
	var paths []string
	for _, outputPath := range bundle.Paths {
		if selected == nil || selected[outputPath] {
			paths = append(paths, outputPath)
		}
	}
	if len(paths) == 0 {
		return ApplyResult{}, nil
	}
	destination, err := openApplyDestinationWithIdentity(destinationRoot, expectedRoot)
	if err != nil {
		return ApplyResult{}, err
	}
	defer destination.Close()
	for _, outputPath := range paths {
		want, ok := baselineByPath[outputPath]
		if !ok {
			return ApplyResult{}, fmt.Errorf("no local baseline for output %q", outputPath)
		}
		got, err := captureBaselineAtRootContext(context.Background(), destination.root, outputPath, outputPath)
		if err != nil {
			return ApplyResult{}, err
		}
		if !sameBaseline(want, got) {
			return ApplyResult{}, fmt.Errorf("output %q conflicts with local changes", outputPath)
		}
	}

	if err := destination.root.Mkdir(transaction, 0o700); err != nil {
		return ApplyResult{}, err
	}
	transactionInfo, err := destination.root.Lstat(transaction)
	if err != nil {
		return ApplyResult{}, err
	}
	transactionIdentity, err := fsidentity.FromInfo(transactionInfo)
	if err != nil {
		return ApplyResult{}, err
	}
	published := false
	defer func() {
		if !published {
			_ = removeApplyTransactionAtRoot(destination.root, transaction, transactionIdentity)
		}
	}()
	if err := syncApplyRootDirectory(destination.root, "."); err != nil {
		return ApplyResult{}, err
	}
	journal := applyJournal{
		Version: applyJournalVersion, Transaction: transaction, TransactionIdentity: transactionIdentity, Owner: owner,
		BundleRoot: bundle.Manifest.RootHash(), Phase: applyPhasePrepared,
		Items: make([]applyJournalItem, len(paths)),
	}
	for i, outputPath := range paths {
		expected := subtreeManifest(bundle.Manifest, outputPath)
		journal.Items[i] = applyJournalItem{
			Path: outputPath, ItemDir: fmt.Sprintf("%06d", i), Original: baselineByPath[outputPath],
			Expected: Baseline{Path: outputPath, Digest: expected.RootHash()}, Phase: applyItemPrepared,
		}
	}
	if err := writeApplyJournalAtRoot(destination.root, journal); err != nil {
		return ApplyResult{}, err
	}
	for i := range journal.Items {
		item := journal.Items[i]
		outputPath := item.Path
		itemDir := item.ItemDir
		itemRoot := path.Join(transaction, itemDir)
		if err := destination.root.Mkdir(itemRoot, 0o700); err != nil {
			return ApplyResult{}, err
		}
		if err := syncApplyRootDirectory(destination.root, transaction); err != nil {
			return ApplyResult{}, err
		}
		value := path.Join(itemRoot, "value")
		if err := copyPathToRoot(filepath.Join(stagedRoot, filepath.FromSlash(outputPath)), destination.root, value); err != nil {
			return ApplyResult{}, err
		}
		got, err := captureBaselineAtRootContext(context.Background(), destination.root, value, "value")
		if err != nil {
			return ApplyResult{}, err
		}
		expected := rebaseManifest(subtreeManifest(bundle.Manifest, outputPath), outputPath, "value")
		if got.Digest != expected.RootHash() {
			return ApplyResult{}, fmt.Errorf("staged output %q changed after download", outputPath)
		}
	}
	published = true

	for i := range journal.Items {
		item := &journal.Items[i]
		if err := destination.verifyPath(); err != nil {
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
		}
		got, err := captureBaselineAtRootContext(context.Background(), destination.root, item.Path, item.Path)
		if err != nil || !sameBaseline(item.Original, got) {
			if err == nil {
				err = fmt.Errorf("output %q conflicts with local changes", item.Path)
			}
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
		}
		if err := ensureOutputParents(destination.root, &journal, item.Path); err != nil {
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
		}
		parentID, err := captureOutputParentIdentity(destination.root, item.Path)
		if err != nil {
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
		}
		destinationDir, err := openOutputParent(destination.root, item.Path, parentID)
		if err != nil {
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
		}
		item.Parent = parentID
		item.Phase = applyItemInstalling
		if err := writeApplyJournalAtRoot(destination.root, journal); err != nil {
			destinationDir.Close()
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
		}
		if err := moveOriginalToBackup(destination.root, journal, *item, destinationDir); err != nil {
			destinationDir.Close()
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
		}
		if err := installPreparedValue(destination.root, journal, *item, destinationDir); err != nil {
			destinationDir.Close()
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
		}
		parentErr := verifyOutputParent(destination.root, item.Path, parentID)
		closeErr := destinationDir.Close()
		if err := errors.Join(parentErr, closeErr); err != nil {
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
		}
		if err := destination.verifyPath(); err != nil {
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
		}
		item.Phase = applyItemInstalled
		if err := writeApplyJournalAtRoot(destination.root, journal); err != nil {
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
		}
	}
	if err := validateApplyBackups(destination.root, journal); err != nil {
		return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
	}
	if err := destination.verifyPath(); err != nil {
		return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
	}
	journal.Phase = applyPhaseCommitted
	if err := writeApplyJournalAtRoot(destination.root, journal); err != nil {
		return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
	}
	if err := destination.verifyPath(); err != nil {
		return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
	}
	return ApplyResult{
		Applied: paths, Transaction: transaction, BundleRoot: journal.BundleRoot,
	}, nil
}

func installPreparedValue(root *os.Root, journal applyJournal, item applyJournalItem, destinationDir *os.File) error {
	sourceDir, err := root.Open(path.Join(journal.Transaction, item.ItemDir))
	if err != nil {
		return err
	}
	defer sourceDir.Close()
	if err := renameNoReplace(sourceDir, "value", destinationDir, path.Base(item.Path)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("output %q conflicts with local changes", item.Path)
		}
		return err
	}
	return errors.Join(sourceDir.Sync(), destinationDir.Sync())
}

func moveOriginalToBackup(
	root *os.Root,
	journal applyJournal,
	item applyJournalItem,
	destinationDir *os.File,
) error {
	backup := path.Join(journal.Transaction, item.ItemDir, "previous")
	backupDir, err := root.Open(path.Dir(backup))
	if err != nil {
		return err
	}
	defer backupDir.Close()
	err = renameNoReplace(destinationDir, path.Base(item.Path), backupDir, path.Base(backup))
	if os.IsNotExist(err) {
		if item.Original.Missing {
			return nil
		}
		return fmt.Errorf("output %q conflicts with local changes", item.Path)
	}
	if err != nil {
		return err
	}
	if err := errors.Join(destinationDir.Sync(), backupDir.Sync()); err != nil {
		return err
	}
	return validateApplyBackup(root, journal, item)
}

func captureOutputParentIdentity(root *os.Root, outputPath string) (fsidentity.Identity, error) {
	if err := rejectSymlinkParentsAtRoot(root, outputPath); err != nil {
		return fsidentity.Identity{}, err
	}
	parent := path.Dir(outputPath)
	info, err := root.Lstat(parent)
	if err != nil {
		return fsidentity.Identity{}, err
	}
	if !info.IsDir() {
		return fsidentity.Identity{}, fmt.Errorf("output path %q passes through non-directory %q", outputPath, parent)
	}
	return fsidentity.FromInfo(info)
}

func openOutputParent(
	root *os.Root,
	outputPath string,
	want fsidentity.Identity,
) (*os.File, error) {
	parent := path.Dir(outputPath)
	dir, err := root.Open(parent)
	if err != nil {
		return nil, err
	}
	info, err := dir.Stat()
	if err != nil {
		dir.Close()
		return nil, err
	}
	got, err := fsidentity.FromInfo(info)
	if err != nil || got != want {
		dir.Close()
		return nil, fmt.Errorf("output %q conflicts with a replaced parent directory", outputPath)
	}
	if err := verifyOutputParent(root, outputPath, want); err != nil {
		dir.Close()
		return nil, err
	}
	return dir, nil
}

func verifyOutputParent(root *os.Root, outputPath string, want fsidentity.Identity) error {
	got, err := captureOutputParentIdentity(root, outputPath)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("output %q conflicts with a replaced parent directory", outputPath)
	}
	return nil
}

func validateApplyBackups(root *os.Root, journal applyJournal) error {
	for _, item := range journal.Items {
		if item.Phase == applyItemPrepared {
			continue
		}
		if err := validateApplyBackup(root, journal, item); err != nil {
			return err
		}
	}
	return nil
}

func validateApplyBackup(root *os.Root, journal applyJournal, item applyJournalItem) error {
	backup := path.Join(journal.Transaction, item.ItemDir, "previous")
	got, err := captureBaselineAtRootContext(context.Background(), root, backup, item.Path)
	if err != nil {
		return err
	}
	if !sameBaselineContent(item.Original, got) {
		return fmt.Errorf("output %q changed while it was being replaced", item.Path)
	}
	return nil
}

func abortApply(root string, journal applyJournal, primary error) error {
	rollbackErr := rollbackApplyJournal(root, journal)
	if rollbackErr == nil {
		rollbackErr = removeApplyTransaction(root, journal)
	}
	if rollbackErr != nil {
		return errors.Join(primary, fmt.Errorf("output rollback incomplete; recovery data retained in %s: %w",
			journal.Transaction, rollbackErr))
	}
	return primary
}

func abortApplyAtRoot(root *os.Root, journal applyJournal, primary error) error {
	rollbackErr := rollbackApplyJournalAtRootContext(context.Background(), root, journal)
	if rollbackErr == nil {
		rollbackErr = removeApplyTransactionAtRoot(root, journal.Transaction, journal.TransactionIdentity)
	}
	if rollbackErr != nil {
		return errors.Join(primary, fmt.Errorf("output rollback incomplete; recovery data retained in %s: %w",
			journal.Transaction, rollbackErr))
	}
	return primary
}

func ensureOutputParents(root *os.Root, journal *applyJournal, outputPath string) error {
	parent := path.Dir(outputPath)
	if parent == "." {
		return nil
	}
	parts := strings.Split(parent, "/")
	for i := range parts {
		current := strings.Join(parts[:i+1], "/")
		info, err := root.Lstat(current)
		if os.IsNotExist(err) {
			stagingRoot := path.Join(journal.Transaction, applyParentStagingDirectory)
			if err := root.Mkdir(stagingRoot, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			staged := path.Join(stagingRoot, fmt.Sprintf("%06d", len(journal.CreatedParents)))
			if err := root.Mkdir(staged, 0o755); err != nil {
				return err
			}
			if err := syncApplyPathAtRoot(root, staged); err != nil {
				return err
			}
			info, err := root.Lstat(staged)
			if err != nil {
				return err
			}
			identity, err := fsidentity.FromInfo(info)
			if err != nil {
				return err
			}
			journal.CreatedParents = append(journal.CreatedParents, applyJournalParent{
				Path: current, Identity: identity,
			})
			if err := writeApplyJournalAtRoot(root, *journal); err != nil {
				return err
			}
			if err := renameApplyPathNoReplace(root, staged, current); err != nil {
				if !errors.Is(err, fs.ErrExist) {
					return err
				}
				info, err = root.Lstat(current)
				if err != nil {
					return err
				}
				if !info.IsDir() {
					return fmt.Errorf("output path %q passes through non-directory %q", outputPath, current)
				}
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("output path %q passes through non-directory %q", outputPath, current)
		}
	}
	return nil
}

func rebaseManifest(manifest proto.Manifest, oldRoot, newRoot string) proto.Manifest {
	rebased := proto.Manifest{Entries: make([]proto.ManifestEntry, len(manifest.Entries))}
	for i, entry := range manifest.Entries {
		entry.Path = newRoot + strings.TrimPrefix(entry.Path, oldRoot)
		rebased.Entries[i] = entry
	}
	return rebased
}

func sameBaseline(a, b Baseline) bool {
	return a.Path == b.Path && a.Missing == b.Missing && a.Digest == b.Digest
}

func sameBaselineContent(a, b Baseline) bool {
	return a.Missing == b.Missing && a.Digest == b.Digest
}

func copyPath(src, dest string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode().IsRegular():
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		syncErr := out.Sync()
		closeErr := out.Close()
		if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
			return err
		}
		return os.Chmod(dest, info.Mode().Perm())
	case info.IsDir():
		if err := os.Mkdir(dest, info.Mode().Perm()|0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyPath(filepath.Join(src, entry.Name()), filepath.Join(dest, entry.Name())); err != nil {
				return err
			}
		}
		if err := os.Chmod(dest, info.Mode().Perm()); err != nil {
			return err
		}
		return syncDirectory(dest)
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dest)
	default:
		return fmt.Errorf("unsupported output type %v at %s", info.Mode(), src)
	}
}

func copyPathToRoot(src string, root *os.Root, dest string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode().IsRegular():
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := root.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		syncErr := out.Sync()
		closeErr := out.Close()
		if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
			return err
		}
		if err := root.Chmod(dest, info.Mode().Perm()); err != nil {
			return err
		}
		return syncApplyPathAtRoot(root, dest)
	case info.IsDir():
		if err := root.Mkdir(dest, info.Mode().Perm()|0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyPathToRoot(filepath.Join(src, entry.Name()), root, path.Join(dest, entry.Name())); err != nil {
				return err
			}
		}
		if err := root.Chmod(dest, info.Mode().Perm()); err != nil {
			return err
		}
		return syncApplyRootDirectory(root, dest)
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := root.Symlink(target, dest); err != nil {
			return err
		}
		return syncApplyPathAtRoot(root, dest)
	default:
		return fmt.Errorf("unsupported output type %v at %s", info.Mode(), src)
	}
}
