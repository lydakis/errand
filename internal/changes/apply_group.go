package changes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"

	"github.com/lydakis/errand/internal/fsidentity"
)

// A group changes existing regular files under verified existing parents. Independent
// item directories retain each backup/value; directory and structural changes
// keep the per-item protocol. See docs/GROUPED_APPLY.md for recovery ordering.
type applyFileGroup struct {
	parents    map[string]fsidentity.Identity
	checkpoint func(string, int) error
}

// Reasons are stable diagnostic categories; never include source paths or bodies.
func planApplyFileGroup(destination *applyDestination, journal applyJournal, inputs map[string]applyPathInput, merged *treeAccess, checkpoint func(string, int) error) (*applyFileGroup, string) {
	if len(journal.Items) < 2 {
		return nil, "single-root"
	}
	parents := make(map[string]fsidentity.Identity)
	for _, item := range journal.Items {
		input := inputs[item.Path]
		if item.MetadataOnly {
			return nil, "metadata"
		}
		if item.Original.Missing {
			return nil, "creation"
		}
		if item.Expected.Missing {
			return nil, "deletion"
		}
		parentPath := path.Dir(item.Path)
		if input.ancestor.path != parentPath || input.ancestor.identity.IsZero() {
			return nil, "missing-parent"
		}
		// The final publication barrier may drain earlier parent member syncs
		// only on the transaction's device. Keep other devices on the reference path.
		if input.ancestor.identity.Device != journal.TransactionIdentity.Device {
			return nil, "cross-device"
		}
		if previous, exists := parents[parentPath]; exists && previous != input.ancestor.identity {
			return nil, "changed-parent"
		}
		parents[parentPath] = input.ancestor.identity
		original, err := destination.root.Lstat(item.Path)
		if err != nil || !original.Mode().IsRegular() {
			return nil, "non-regular-original"
		}
		value, err := merged.root.Lstat(item.Path)
		if err != nil || !value.Mode().IsRegular() {
			return nil, "non-regular-value"
		}
		// Group installation/recovery uses final physical modes directly.
		if value.Mode().Perm() != merged.original[item.Path] {
			return nil, "widened-mode"
		}
	}
	return &applyFileGroup{parents: parents, checkpoint: checkpoint}, "eligible"
}

func (g *applyFileGroup) check(event string, index int) error {
	if g.checkpoint != nil {
		return g.checkpoint(event, index)
	}
	return nil
}

func (g *applyFileGroup) install(destination *applyDestination, journal *applyJournal, synchronization applySynchronization) error {
	// Only one installation parent is held at a time, even for deep/mixed trees.
	var parent *os.File
	var parentPath string
	defer func() {
		if parent != nil {
			_ = parent.Close()
		}
	}()
	for i := range journal.Items {
		journal.Items[i].Parent = g.parents[path.Dir(journal.Items[i].Path)]
		journal.Items[i].Phase = applyItemGrouped
	}
	if err := writeApplyJournalAtRoot(destination.root, *journal); err != nil {
		return err
	}
	if err := g.check("intent", -1); err != nil {
		return err
	}
	verify := func(item applyJournalItem) error {
		return errors.Join(destination.verifyPath(), verifyChangeParent(destination.root, item.Path, item.Parent))
	}
	for i, item := range journal.Items {
		nextParent := path.Dir(item.Path)
		if parent == nil || nextParent != parentPath {
			if parent != nil {
				err := parent.Close()
				parent = nil
				if err != nil {
					return err
				}
			}
			var err error
			parent, err = openChangeParent(destination.root, item.Path, item.Parent)
			if err != nil {
				return err
			}
			parentPath = nextParent
		}
		if err := g.check("before-backup", i); err != nil {
			return err
		}
		if err := verify(item); err != nil {
			return err
		}
		got, _, _, err := captureApplyItemBaseline(context.Background(), destination.root, item)
		if err != nil {
			return err
		}
		if !sameBaseline(item.Original, got) {
			return fmt.Errorf("change %q conflicts with local changes", item.Path)
		}
		if err := g.backup(destination, *journal, item, parent, i, synchronization); err != nil {
			return err
		}

		if err := g.check("backups-durable", i); err != nil {
			return err
		}
		if err := verify(item); err != nil {
			return err
		}
		sourceDir, err := renamePreparedValue(destination.root, *journal, item, parent)
		if err != nil {
			return err
		}
		err = g.check("install-rename", i)
		if err == nil {
			err = synchronization.member("install-directory", sourceDir)
		}
		if err := errors.Join(err, sourceDir.Close()); err != nil {
			return err
		}

	}
	if err := parent.Close(); err != nil {
		parent = nil
		return err
	}
	parent = nil
	if err := destination.verifyPath(); err != nil {
		return err
	}
	if err := g.check("install-barrier", -1); err != nil {
		return err
	}
	// Fsync every parent: syncing another directory cannot publish its entries.
	// Darwin needs one final same-device cache drain after those member fsyncs.
	// Any parent can carry it; choose the last to avoid an extra synchronization.
	// Linux still fsyncs each directory. Backup barriers are unchanged.
	names := make([]string, 0, len(g.parents))
	for name := range g.parents {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		dir, err := openApplyDirectory(destination.root, name, g.parents[name])
		if err != nil {
			return err
		}
		if i == len(names)-1 {
			err = synchronization.barrier("install-parent", dir)
		} else {
			err = synchronization.member("install-parent", dir)
		}
		if err = errors.Join(err, dir.Close()); err != nil {
			return err
		}
		if err := g.check("parent-published", i); err != nil {
			return err
		}
	}
	if err := g.check("installed", -1); err != nil {
		return err
	}
	for i := range journal.Items {
		journal.Items[i].Phase = applyItemInstalled
	}
	return nil
}

// Keep each backup/replacement pair together. The backup data and renamed entry
// must both be durable before installing that file; other files stay visible.
func (g *applyFileGroup) backup(destination *applyDestination, journal applyJournal, item applyJournalItem, parent *os.File, index int, synchronization applySynchronization) error {
	dir, err := renameOriginalToBackup(destination.root, journal, item, parent)
	if err != nil {
		return err
	}
	if dir == nil {
		return fmt.Errorf("grouped original %q disappeared", item.Path)
	}
	defer dir.Close()
	if err := g.check("backup-rename", index); err != nil {
		return err
	}
	if err := synchronizeApplyBackup(destination.root, journal, item, synchronization); err != nil {
		return err
	}
	if err := synchronization.member("backup-directory", dir); err != nil {
		return err
	}
	if err := g.check("backup-barrier", index); err != nil {
		return err
	}
	return synchronization.barrier("backup-parent", parent)
}
