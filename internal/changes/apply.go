package changes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lydakis/errand/internal/archive"
	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

type applyAncestorGuard struct {
	path     string
	identity fsidentity.Identity
}

type applyPathInput struct {
	original     Baseline
	ancestor     applyAncestorGuard
	metadataOnly bool
	identity     fsidentity.Identity
	mode         uint32
}

type applyParentPolicy struct {
	mustExist bool
	mode      os.FileMode
}

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
		return nil, fmt.Errorf("local change workspace root is not a directory")
	}
	if identity != expected {
		return nil, fmt.Errorf("local change workspace at %q is not the workspace that submitted the job", destinationRoot)
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
		return nil, fmt.Errorf("local change workspace root changed while application started")
	}
	return &applyDestination{path: destinationRoot, root: root, identity: identity}, nil
}

func applyWorkspaceIdentity(destinationRoot string) (fsidentity.Identity, error) {
	identity, info, err := fsidentity.Lstat(destinationRoot)
	if err != nil {
		return fsidentity.Identity{}, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return fsidentity.Identity{}, fmt.Errorf("local change workspace root is not a directory")
	}
	return identity, nil
}

func (d *applyDestination) Close() error {
	return d.root.Close()
}

func (d *applyDestination) verifyPath() error {
	identity, info, err := fsidentity.Lstat(d.path)
	if err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || identity != d.identity {
		return fmt.Errorf("local change workspace root changed during application")
	}
	return nil
}

// Apply prepares and installs one set of changes, leaving a durable transaction
// journal until the caller records its local apply state and calls CommitApply.
func Apply(
	stagedRoot string,
	destinationRoot string,
	bundle proto.ChangeBundle,
	selected map[string]bool,
	owner string,
	transaction string,
) (ApplyResult, error) {
	identity, err := applyWorkspaceIdentity(destinationRoot)
	if err != nil {
		return ApplyResult{}, err
	}
	return ApplyToWorkspace(
		stagedRoot, destinationRoot, bundle, selected, owner, transaction, identity,
	)
}

// ApplyToWorkspace applies changes only if destinationRoot still identifies
// the workspace recorded when the job was submitted.
func ApplyToWorkspace(
	stagedRoot string,
	destinationRoot string,
	bundle proto.ChangeBundle,
	selected map[string]bool,
	owner string,
	transaction string,
	expectedRoot fsidentity.Identity,
) (ApplyResult, error) {
	if err := validateBundle(bundle); err != nil {
		return ApplyResult{}, err
	}
	if owner == "" {
		return ApplyResult{}, fmt.Errorf("change apply owner is required")
	}
	if !validApplyTransaction(transaction) {
		return ApplyResult{}, fmt.Errorf("invalid change transaction name %q", transaction)
	}
	var paths []string
	for _, changePath := range bundle.Paths {
		if selected == nil || selected[changePath] {
			paths = append(paths, changePath)
		}
	}
	if len(paths) == 0 {
		return ApplyResult{}, nil
	}
	reportedPaths := append([]string(nil), paths...)
	metadataPaths := make(map[string]bool, len(bundle.MetadataPaths))
	for _, metadataPath := range bundle.MetadataPaths {
		metadataPaths[metadataPath] = true
	}
	sort.SliceStable(paths, func(i, j int) bool {
		if metadataPaths[paths[i]] != metadataPaths[paths[j]] {
			return !metadataPaths[paths[i]]
		}
		if metadataPaths[paths[i]] {
			return strings.Count(paths[i], "/") > strings.Count(paths[j], "/")
		}
		return paths[i] < paths[j]
	})
	destination, err := openApplyDestinationWithIdentity(destinationRoot, expectedRoot)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("opening apply destination: %w", err)
	}
	defer destination.Close()
	parentPolicies, err := applyParentPolicies(destination.root, bundle, paths)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("checking apply parents: %w", err)
	}
	mergeRoot, err := os.MkdirTemp(filepath.Dir(stagedRoot), ".merge-")
	if err != nil {
		return ApplyResult{}, err
	}
	defer RemoveTree(mergeRoot)
	oursRoot := filepath.Join(mergeRoot, "ours")
	mergedRoot := filepath.Join(mergeRoot, "merged")
	trustedRoot := filepath.Join(mergeRoot, "trusted")
	if err := os.Mkdir(oursRoot, 0o700); err != nil {
		return ApplyResult{}, err
	}
	if err := os.Mkdir(mergedRoot, 0o700); err != nil {
		return ApplyResult{}, err
	}
	if err := os.Mkdir(trustedRoot, 0o700); err != nil {
		return ApplyResult{}, err
	}
	trustedAccess, err := materializeVerifiedMergeInputs(stagedRoot, trustedRoot, bundle)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("verifying staged changes: %w", err)
	}
	defer closeTreeAccesses(trustedAccess)
	oursManifest, inputs, err := captureApplyInputs(destination, bundle, paths)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("capturing local merge input: %w", err)
	}
	oursAccess, err := materializeApplySnapshotStrict(destination.path, oursRoot, mergeRoot, oursManifest)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("materializing local merge input: %w", err)
	}
	defer oursAccess.closeWithoutRestore()
	if err := mergeChangeRoots(
		context.Background(),
		filepath.Join(trustedRoot, "base"),
		oursRoot,
		filepath.Join(trustedRoot, "remote"),
		mergedRoot,
		bundle,
		paths,
		oursManifest,
	); err != nil {
		return ApplyResult{}, fmt.Errorf("merging workspace changes: %w", err)
	}
	mergedAccess, err := makeTreeAccessible(mergedRoot)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("preparing merged workspace: %w", err)
	}
	defer mergedAccess.closeWithoutRestore()

	if err := destination.root.Mkdir(transaction, 0o700); err != nil {
		return ApplyResult{}, fmt.Errorf("creating apply transaction: %w", err)
	}
	transactionInfo, err := destination.root.Lstat(transaction)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("inspecting apply transaction: %w", err)
	}
	transactionIdentity, err := fsidentity.FromInfo(transactionInfo)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("identifying apply transaction: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = removeApplyTransactionAtRoot(destination.root, transaction, transactionIdentity)
		}
	}()
	if err := syncApplyRootDirectory(destination.root, "."); err != nil {
		return ApplyResult{}, fmt.Errorf("syncing apply transaction: %w", err)
	}
	journal := applyJournal{
		Version: applyJournalVersion, Transaction: transaction, TransactionIdentity: transactionIdentity, Owner: owner,
		BundleRoot: bundle.RootHash(), Phase: applyPhasePrepared,
		Items: make([]applyJournalItem, len(paths)),
	}
	for i, changePath := range paths {
		input := inputs[changePath]
		var expected Baseline
		if input.metadataOnly {
			mode, ok := mergedAccess.original[changePath]
			if !ok {
				return ApplyResult{}, fmt.Errorf("merged metadata change %q is missing", changePath)
			}
			expected = metadataBaseline(changePath, uint32(mode))
		} else {
			expected, err = captureBaselineWithAccess(
				context.Background(), mergedAccess, changePath, changePath,
			)
			if err != nil {
				return ApplyResult{}, fmt.Errorf("capturing merged change %q: %w", changePath, err)
			}
		}
		journal.Items[i] = applyJournalItem{
			Path: changePath, ItemDir: fmt.Sprintf("%06d", i), Original: input.original,
			Expected: expected, Phase: applyItemPrepared, MetadataOnly: input.metadataOnly,
			Target: input.identity, OriginalMode: input.mode,
		}
		if input.metadataOnly {
			journal.Items[i].ExpectedMode = uint32(mergedAccess.original[changePath])
		}
	}
	if err := writeApplyJournalAtRoot(destination.root, journal); err != nil {
		return ApplyResult{}, fmt.Errorf("writing apply journal: %w", err)
	}
	for i := range journal.Items {
		item := journal.Items[i]
		changePath := item.Path
		itemDir := item.ItemDir
		itemRoot := path.Join(transaction, itemDir)
		if err := destination.root.Mkdir(itemRoot, 0o700); err != nil {
			return ApplyResult{}, fmt.Errorf("creating apply item for %q: %w", changePath, err)
		}
		if err := syncApplyRootDirectory(destination.root, transaction); err != nil {
			return ApplyResult{}, fmt.Errorf("syncing apply item for %q: %w", changePath, err)
		}
		if item.Expected.Missing || item.MetadataOnly {
			continue
		}
		value := path.Join(itemRoot, "value")
		if err := copyPathToRoot(
			mergedRoot, changePath, destination.root, value,
		); err != nil {
			return ApplyResult{}, fmt.Errorf("staging merged change %q: %w", changePath, err)
		}
		got, err := captureBaselineWithLogicalModesAtRoot(
			context.Background(), destination.root, value, item.Path, mergedAccess.original,
		)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("verifying staged merged change %q: %w", changePath, err)
		}
		if !sameBaselineContent(got, item.Expected) {
			return ApplyResult{}, fmt.Errorf("merged change %q changed before installation", changePath)
		}
	}
	published = true

	for i := range journal.Items {
		item := &journal.Items[i]
		if err := destination.verifyPath(); err != nil {
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
		}
		got, identity, _, err := captureApplyItemBaseline(context.Background(), destination.root, *item)
		if err != nil || !sameBaseline(item.Original, got) {
			if err == nil {
				err = fmt.Errorf("change %q conflicts with local changes", item.Path)
			} else {
				err = fmt.Errorf("checking local change %q: %w", item.Path, err)
			}
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
		}
		if item.MetadataOnly && identity != item.Target {
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal,
				fmt.Errorf("metadata change %q conflicts with a replaced directory", item.Path))
		}
		if input := inputs[item.Path]; !input.ancestor.identity.IsZero() {
			if err := verifyApplyAncestor(destination.root, input.ancestor); err != nil {
				return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
			}
		}
		if err := ensureChangeParents(destination.root, &journal, item.Path, parentPolicies); err != nil {
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal,
				fmt.Errorf("preparing parents for %q: %w", item.Path, err))
		}
		parentID, err := captureChangeParentIdentity(destination.root, item.Path)
		if err != nil {
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal,
				fmt.Errorf("identifying parent for %q: %w", item.Path, err))
		}
		destinationDir, err := openChangeParent(destination.root, item.Path, parentID)
		if err != nil {
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
		}
		item.Parent = parentID
		item.Phase = applyItemInstalling
		if err := writeApplyJournalAtRoot(destination.root, journal); err != nil {
			destinationDir.Close()
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
		}
		if item.MetadataOnly {
			if err := installMetadataChange(destination.root, *item); err != nil {
				destinationDir.Close()
				return ApplyResult{}, abortApplyAtRoot(destination.root, journal,
					fmt.Errorf("installing metadata for %q: %w", item.Path, err))
			}
		} else if err := moveOriginalToBackup(destination.root, journal, *item, destinationDir); err != nil {
			destinationDir.Close()
			return ApplyResult{}, abortApplyAtRoot(destination.root, journal,
				fmt.Errorf("backing up %q: %w", item.Path, err))
		} else if !item.Expected.Missing {
			if err := installPreparedValue(destination.root, journal, *item, destinationDir); err != nil {
				destinationDir.Close()
				return ApplyResult{}, abortApplyAtRoot(destination.root, journal,
					fmt.Errorf("installing %q: %w", item.Path, err))
			}
			if err := restoreLogicalModesAtRoot(destination.root, item.Path, mergedAccess.original); err != nil {
				destinationDir.Close()
				return ApplyResult{}, abortApplyAtRoot(destination.root, journal,
					fmt.Errorf("restoring logical modes for %q: %w", item.Path, err))
			}
		}
		parentErr := verifyChangeParent(destination.root, item.Path, parentID)
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
		return ApplyResult{}, abortApplyAtRoot(destination.root, journal,
			fmt.Errorf("validating apply backups: %w", err))
	}
	if err := validateInstalledChanges(destination.root, journal); err != nil {
		return ApplyResult{}, abortApplyAtRoot(destination.root, journal,
			fmt.Errorf("validating installed changes: %w", err))
	}
	if err := destination.verifyPath(); err != nil {
		return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
	}
	journal.Phase = applyPhaseCommitted
	if err := writeApplyJournalAtRoot(destination.root, journal); err != nil {
		return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
	}
	if err := validateInstalledChanges(destination.root, journal); err != nil {
		return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
	}
	if err := destination.verifyPath(); err != nil {
		return ApplyResult{}, abortApplyAtRoot(destination.root, journal, err)
	}
	return ApplyResult{
		Applied: reportedPaths, States: applyJournalStates(journal), Transaction: transaction, BundleRoot: journal.BundleRoot,
	}, nil
}

func captureApplyInputs(
	destination *applyDestination,
	bundle proto.ChangeBundle,
	paths []string,
) (proto.Manifest, map[string]applyPathInput, error) {
	manifest := proto.Manifest{}
	inputs := make(map[string]applyPathInput, len(paths))
	metadata := make(map[string]bool, len(bundle.MetadataPaths))
	for _, changePath := range bundle.MetadataPaths {
		metadata[changePath] = true
	}
	remainingBytes := proto.DefaultLimits().MaxChangeBytes
	remainingEntries := MaxChangeEntries
	for _, changePath := range paths {
		input := applyPathInput{metadataOnly: metadata[changePath]}
		if input.metadataOnly {
			if remainingEntries <= 0 {
				return proto.Manifest{}, nil, fmt.Errorf("%w: local changes exceed %d entries", ErrEntryLimitExceeded, MaxChangeEntries)
			}
			baseline, identity, mode, err := captureMetadataBaselineAtRoot(
				context.Background(), destination.root, changePath, changePath,
			)
			if err != nil {
				if os.IsNotExist(err) {
					err = &MergeConflictError{Paths: []string{changePath}}
				}
				return proto.Manifest{}, nil, err
			}
			input.original, input.identity, input.mode = baseline, identity, mode
			manifest.Entries = append(manifest.Entries, proto.ManifestEntry{
				Path: changePath, Type: proto.EntryDir, Mode: mode,
			})
			remainingEntries--
		} else {
			subtree, missing, bytes, entries, err := captureManifestAtRootBoundedContext(
				context.Background(), destination.root, changePath, changePath,
				remainingBytes, remainingEntries,
			)
			if err != nil {
				return proto.Manifest{}, nil, err
			}
			if missing {
				input.original = Baseline{Path: changePath, Missing: true}
			} else {
				input.original = Baseline{Path: changePath, Digest: subtree.RootHash()}
				manifest.Entries = append(manifest.Entries, subtree.Entries...)
				remainingBytes -= bytes
				remainingEntries -= entries
			}
		}
		ancestor, err := captureApplyAncestor(destination.root, changePath)
		if err != nil {
			return proto.Manifest{}, nil, err
		}
		input.ancestor = ancestor
		inputs[changePath] = input
	}
	return manifest, inputs, nil
}

func metadataBaseline(changePath string, mode uint32) Baseline {
	manifest := proto.Manifest{Entries: []proto.ManifestEntry{{
		Path: changePath, Type: proto.EntryDir, Mode: mode,
	}}}
	return Baseline{Path: changePath, Digest: manifest.RootHash()}
}

func captureMetadataBaselineAtRoot(
	ctx context.Context,
	root *os.Root,
	rel string,
	logicalPath string,
) (Baseline, fsidentity.Identity, uint32, error) {
	if err := ctx.Err(); err != nil {
		return Baseline{}, fsidentity.Identity{}, 0, err
	}
	if err := validatePath(rel); err != nil {
		return Baseline{}, fsidentity.Identity{}, 0, err
	}
	if err := validatePath(logicalPath); err != nil {
		return Baseline{}, fsidentity.Identity{}, 0, err
	}
	if err := rejectSymlinkParentsAtRoot(root, rel); err != nil {
		return Baseline{}, fsidentity.Identity{}, 0, err
	}
	info, err := root.Lstat(rel)
	if err != nil {
		return Baseline{}, fsidentity.Identity{}, 0, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return Baseline{}, fsidentity.Identity{}, 0, &MergeConflictError{Paths: []string{logicalPath}}
	}
	identity, err := fsidentity.FromInfo(info)
	if err != nil {
		return Baseline{}, fsidentity.Identity{}, 0, err
	}
	mode := uint32(info.Mode().Perm())
	return metadataBaseline(logicalPath, mode), identity, mode, nil
}

func captureApplyItemBaseline(
	ctx context.Context,
	root *os.Root,
	item applyJournalItem,
) (Baseline, fsidentity.Identity, uint32, error) {
	if item.MetadataOnly {
		return captureMetadataBaselineAtRoot(ctx, root, item.Path, item.Path)
	}
	baseline, err := captureBaselineAtRootContext(ctx, root, item.Path, item.Path)
	return baseline, fsidentity.Identity{}, 0, err
}

func installMetadataChange(root *os.Root, item applyJournalItem) error {
	current, identity, mode, err := captureMetadataBaselineAtRoot(
		context.Background(), root, item.Path, item.Path,
	)
	if err != nil {
		return err
	}
	if identity != item.Target || mode != item.OriginalMode || !sameBaseline(current, item.Original) {
		return fmt.Errorf("metadata change %q conflicts with local changes", item.Path)
	}
	if err := root.Chmod(item.Path, os.FileMode(item.ExpectedMode)); err != nil {
		return err
	}
	installed, installedIdentity, installedMode, err := captureMetadataBaselineAtRoot(
		context.Background(), root, item.Path, item.Path,
	)
	if err != nil || installedIdentity != item.Target || installedMode != item.ExpectedMode ||
		!sameBaselineContent(installed, item.Expected) {
		return errors.Join(fmt.Errorf("metadata change %q changed while it was installed", item.Path), err)
	}
	dir, err := root.Open(item.Path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func materializeApplySnapshot(sourceRoot, destinationRoot, tempRoot string, manifest proto.Manifest) (*treeAccess, error) {
	archiveFile, err := os.CreateTemp(tempRoot, "ours-*.tar")
	if err != nil {
		return nil, err
	}
	archivePath := archiveFile.Name()
	defer os.Remove(archivePath)
	sourceAccess, err := makeManifestAccessible(sourceRoot, manifest)
	if err != nil {
		archiveFile.Close()
		return nil, err
	}
	packErr := snapshot.PackContextWithPhysicalModes(
		context.Background(), archiveFile, sourceRoot, manifest, sourceAccess.physical,
	)
	if err := errors.Join(packErr, sourceAccess.restore()); err != nil {
		archiveFile.Close()
		return nil, err
	}
	if _, err := archiveFile.Seek(0, io.SeekStart); err != nil {
		archiveFile.Close()
		return nil, err
	}
	if err := archive.Extract(
		archiveFile, destinationRoot, manifest, manifestBytes(manifest),
	); err != nil {
		archiveFile.Close()
		return nil, err
	}
	if err := archiveFile.Close(); err != nil {
		return nil, err
	}
	return makeTreeAccessible(destinationRoot)
}

func materializeApplySnapshotStrict(sourceRoot, destinationRoot, tempRoot string, manifest proto.Manifest) (*treeAccess, error) {
	archiveFile, err := os.CreateTemp(tempRoot, "ours-*.tar")
	if err != nil {
		return nil, err
	}
	archivePath := archiveFile.Name()
	defer os.Remove(archivePath)
	if err := snapshot.PackContext(context.Background(), archiveFile, sourceRoot, manifest); err != nil {
		archiveFile.Close()
		return nil, err
	}
	if _, err := archiveFile.Seek(0, io.SeekStart); err != nil {
		archiveFile.Close()
		return nil, err
	}
	if err := archive.Extract(archiveFile, destinationRoot, manifest, manifestBytes(manifest)); err != nil {
		archiveFile.Close()
		return nil, err
	}
	if err := archiveFile.Close(); err != nil {
		return nil, err
	}
	return makeTreeAccessible(destinationRoot)
}

func materializeVerifiedMergeInputs(stagedRoot, destinationRoot string, bundle proto.ChangeBundle) ([]*treeAccess, error) {
	var accesses []*treeAccess
	for _, tree := range []struct {
		name     string
		manifest proto.Manifest
	}{
		{name: "base", manifest: bundle.BaseManifest},
		{name: "remote", manifest: bundle.RemoteManifest},
	} {
		dest := filepath.Join(destinationRoot, tree.name)
		if err := os.Mkdir(dest, 0o700); err != nil {
			closeTreeAccesses(accesses)
			return nil, err
		}
		access, err := materializeApplySnapshot(
			filepath.Join(stagedRoot, tree.name), dest, destinationRoot, tree.manifest,
		)
		if err != nil {
			closeTreeAccesses(accesses)
			return nil, err
		}
		accesses = append(accesses, access)
	}
	return accesses, nil
}

func closeTreeAccesses(accesses []*treeAccess) {
	for _, access := range accesses {
		_ = access.closeWithoutRestore()
	}
}

func captureBaselineWithAccess(
	ctx context.Context,
	access *treeAccess,
	rel string,
	logicalPath string,
) (Baseline, error) {
	manifest, missing, _, _, err := captureManifestAtRootBoundedContext(
		ctx, access.root, rel, logicalPath,
		proto.DefaultLimits().MaxChangeBytes, MaxChangeEntries,
	)
	if err != nil {
		return Baseline{}, err
	}
	if missing {
		return Baseline{Path: logicalPath, Missing: true}, nil
	}
	access.logicalizeRebased(&manifest, rel, logicalPath)
	return Baseline{Path: logicalPath, Digest: manifest.RootHash()}, nil
}

func captureBaselineWithLogicalModesAtRoot(
	ctx context.Context,
	root *os.Root,
	rel string,
	logicalPath string,
	logicalModes map[string]fs.FileMode,
) (Baseline, error) {
	manifest, missing, _, _, err := captureManifestAtRootBoundedContext(
		ctx, root, rel, logicalPath,
		proto.DefaultLimits().MaxChangeBytes, MaxChangeEntries,
	)
	if err != nil {
		return Baseline{}, err
	}
	if missing {
		return Baseline{Path: logicalPath, Missing: true}, nil
	}
	for i := range manifest.Entries {
		if mode, ok := logicalModes[manifest.Entries[i].Path]; ok {
			manifest.Entries[i].Mode = uint32(mode)
		}
	}
	return Baseline{Path: logicalPath, Digest: manifest.RootHash()}, nil
}

func restoreLogicalModesAtRoot(root *os.Root, changePath string, logicalModes map[string]fs.FileMode) error {
	prefix := changePath + "/"
	var paths []string
	for entryPath := range logicalModes {
		if entryPath == changePath || strings.HasPrefix(entryPath, prefix) {
			paths = append(paths, entryPath)
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		leftDepth := strings.Count(paths[i], "/")
		rightDepth := strings.Count(paths[j], "/")
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		return paths[i] > paths[j]
	})
	for _, entryPath := range paths {
		info, err := root.Lstat(entryPath)
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		if err := root.Chmod(entryPath, logicalModes[entryPath]); err != nil {
			return err
		}
	}
	return nil
}

func captureApplyAncestor(root *os.Root, changePath string) (applyAncestorGuard, error) {
	for current := path.Dir(changePath); ; current = path.Dir(current) {
		info, err := root.Lstat(current)
		if os.IsNotExist(err) {
			if current == "." {
				return applyAncestorGuard{}, err
			}
			continue
		}
		if err != nil {
			return applyAncestorGuard{}, err
		}
		if !info.IsDir() {
			return applyAncestorGuard{}, fmt.Errorf("change path %q passes through non-directory %q", changePath, current)
		}
		identity, err := fsidentity.FromInfo(info)
		if err != nil {
			return applyAncestorGuard{}, err
		}
		return applyAncestorGuard{path: current, identity: identity}, nil
	}
}

func applyParentPolicies(root *os.Root, bundle proto.ChangeBundle, paths []string) (map[string]applyParentPolicy, error) {
	base := make(map[string]proto.ManifestEntry, len(bundle.BaseManifest.Entries))
	for _, entry := range bundle.BaseManifest.Entries {
		base[entry.Path] = entry
	}
	remote := make(map[string]proto.ManifestEntry, len(bundle.RemoteManifest.Entries))
	for _, entry := range bundle.RemoteManifest.Entries {
		remote[entry.Path] = entry
	}
	policies := make(map[string]applyParentPolicy)
	conflicts := make(map[string]bool)
	for _, changePath := range paths {
		for current := path.Dir(changePath); current != "."; current = path.Dir(current) {
			policy := policies[current]
			if entry, ok := base[current]; ok && entry.Type == proto.EntryDir {
				policy.mustExist = true
				policy.mode = os.FileMode(entry.Mode)
			}
			if entry, ok := remote[current]; ok && entry.Type == proto.EntryDir {
				policy.mode = os.FileMode(entry.Mode)
			}
			policies[current] = policy
			if !policy.mustExist {
				continue
			}
			info, err := root.Lstat(current)
			switch {
			case os.IsNotExist(err):
				conflicts[current] = true
			case err != nil:
				return nil, err
			case !info.IsDir():
				conflicts[current] = true
			}
		}
	}
	if len(conflicts) != 0 {
		conflictPaths := make([]string, 0, len(conflicts))
		for conflictPath := range conflicts {
			conflictPaths = append(conflictPaths, conflictPath)
		}
		sort.Strings(conflictPaths)
		return nil, &MergeConflictError{Paths: conflictPaths}
	}
	return policies, nil
}

func verifyApplyAncestor(root *os.Root, guard applyAncestorGuard) error {
	info, err := root.Lstat(guard.path)
	if err != nil {
		return err
	}
	identity, err := fsidentity.FromInfo(info)
	if err != nil || !info.IsDir() || identity != guard.identity {
		return fmt.Errorf("local change ancestor %q was replaced during application", guard.path)
	}
	return nil
}

func applyJournalStates(journal applyJournal) map[string]string {
	states := make(map[string]string, len(journal.Items))
	for _, item := range journal.Items {
		states[item.Path] = applyItemState(item)
	}
	return states
}

func applyItemState(item applyJournalItem) string {
	state := baselineState(item.Expected)
	if item.MetadataOnly {
		return metadataStatePrefix + state
	}
	return state
}

func baselineState(baseline Baseline) string {
	if baseline.Missing {
		return "missing"
	}
	return baseline.Digest
}

func installPreparedValue(root *os.Root, journal applyJournal, item applyJournalItem, destinationDir *os.File) error {
	sourceDir, err := root.Open(path.Join(journal.Transaction, item.ItemDir))
	if err != nil {
		return err
	}
	defer sourceDir.Close()
	value := path.Join(journal.Transaction, item.ItemDir, "value")
	if err := renameNoReplacePreservingDirectoryMode(
		root, value, item.Path, sourceDir, "value", destinationDir, path.Base(item.Path),
	); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("change %q conflicts with local changes", item.Path)
		}
		return fmt.Errorf("renaming prepared value: %w", err)
	}
	if err := sourceDir.Sync(); err != nil {
		return fmt.Errorf("syncing prepared value directory: %w", err)
	}
	if err := destinationDir.Sync(); err != nil {
		return fmt.Errorf("syncing change destination directory: %w", err)
	}
	return nil
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
	err = renameNoReplacePreservingDirectoryMode(
		root, item.Path, backup, destinationDir, path.Base(item.Path), backupDir, path.Base(backup),
	)
	if os.IsNotExist(err) {
		if item.Original.Missing {
			return nil
		}
		return fmt.Errorf("change %q conflicts with local changes", item.Path)
	}
	if err != nil {
		return err
	}
	if err := errors.Join(destinationDir.Sync(), backupDir.Sync()); err != nil {
		return err
	}
	return validateApplyBackup(root, journal, item)
}

func captureChangeParentIdentity(root *os.Root, changePath string) (fsidentity.Identity, error) {
	if err := rejectSymlinkParentsAtRoot(root, changePath); err != nil {
		return fsidentity.Identity{}, err
	}
	parent := path.Dir(changePath)
	info, err := root.Lstat(parent)
	if err != nil {
		return fsidentity.Identity{}, err
	}
	if !info.IsDir() {
		return fsidentity.Identity{}, fmt.Errorf("change path %q passes through non-directory %q", changePath, parent)
	}
	return fsidentity.FromInfo(info)
}

func openChangeParent(
	root *os.Root,
	changePath string,
	want fsidentity.Identity,
) (*os.File, error) {
	parent := path.Dir(changePath)
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
		return nil, fmt.Errorf("change %q conflicts with a replaced parent directory", changePath)
	}
	if err := verifyChangeParent(root, changePath, want); err != nil {
		dir.Close()
		return nil, err
	}
	return dir, nil
}

func verifyChangeParent(root *os.Root, changePath string, want fsidentity.Identity) error {
	got, err := captureChangeParentIdentity(root, changePath)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("change %q conflicts with a replaced parent directory", changePath)
	}
	return nil
}

func validateApplyBackups(root *os.Root, journal applyJournal) error {
	for _, item := range journal.Items {
		if item.Phase == applyItemPrepared || item.MetadataOnly {
			continue
		}
		if err := validateApplyBackup(root, journal, item); err != nil {
			return err
		}
	}
	return nil
}

func validateInstalledChanges(root *os.Root, journal applyJournal) error {
	for _, item := range journal.Items {
		if item.Phase != applyItemInstalled {
			continue
		}
		got, identity, mode, err := captureApplyItemBaseline(context.Background(), root, item)
		if err != nil {
			return err
		}
		if !sameBaselineContent(item.Expected, got) ||
			(item.MetadataOnly && (identity != item.Target || mode != item.ExpectedMode)) {
			return fmt.Errorf("change %q changed after installation", item.Path)
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
		return fmt.Errorf("change %q changed while it was being replaced", item.Path)
	}
	return nil
}

func abortApply(root string, journal applyJournal, primary error) error {
	rollbackErr := rollbackApplyJournal(root, journal)
	if rollbackErr == nil {
		rollbackErr = removeApplyTransaction(root, journal)
	}
	if rollbackErr != nil {
		return errors.Join(primary, fmt.Errorf("change rollback incomplete; recovery data retained in %s: %w",
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
		return errors.Join(primary, fmt.Errorf("change rollback incomplete; recovery data retained in %s: %w",
			journal.Transaction, rollbackErr))
	}
	return primary
}

func ensureChangeParents(
	root *os.Root,
	journal *applyJournal,
	changePath string,
	policies map[string]applyParentPolicy,
) error {
	parent := path.Dir(changePath)
	if parent == "." {
		return nil
	}
	parts := strings.Split(parent, "/")
	for i := range parts {
		current := strings.Join(parts[:i+1], "/")
		info, err := root.Lstat(current)
		if os.IsNotExist(err) {
			policy, knownPolicy := policies[current]
			if policy.mustExist {
				return &MergeConflictError{Paths: []string{current}}
			}
			mode := os.FileMode(0o700)
			if knownPolicy {
				mode = policy.mode.Perm()
			}
			stagingRoot := path.Join(journal.Transaction, applyParentStagingDirectory)
			if err := root.Mkdir(stagingRoot, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			staged := path.Join(stagingRoot, fmt.Sprintf("%06d", len(journal.CreatedParents)))
			if err := root.Mkdir(staged, mode|0o700); err != nil {
				return err
			}
			if err := root.Chmod(staged, mode); err != nil {
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
					return fmt.Errorf("change path %q passes through non-directory %q", changePath, current)
				}
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("change path %q passes through non-directory %q", changePath, current)
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
		return fmt.Errorf("unsupported change type %v at %s", info.Mode(), src)
	}
}

func copyPathToRoot(
	sourceRoot string,
	sourceRel string,
	root *os.Root,
	dest string,
) error {
	src := filepath.Join(sourceRoot, filepath.FromSlash(sourceRel))
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	mode := info.Mode().Perm()
	switch {
	case info.Mode().IsRegular():
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := root.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode|0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		syncErr := out.Sync()
		closeErr := out.Close()
		if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
			return err
		}
		if err := root.Chmod(dest, mode); err != nil {
			return err
		}
		return syncApplyPathAtRoot(root, dest)
	case info.IsDir():
		if err := root.Mkdir(dest, mode|0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyPathToRoot(
				sourceRoot, path.Join(sourceRel, entry.Name()),
				root, path.Join(dest, entry.Name()),
			); err != nil {
				return err
			}
		}
		dir, err := root.Open(dest)
		if err != nil {
			return err
		}
		if err := root.Chmod(dest, mode); err != nil {
			dir.Close()
			return err
		}
		return errors.Join(dir.Sync(), dir.Close())
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
		return fmt.Errorf("unsupported change type %v at %s", info.Mode(), src)
	}
}
