package changes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"

	"github.com/lydakis/errand/internal/fsidentity"
)

// A group changes existing regular files under one pinned parent. Independent
// item directories retain each backup/value; directory and structural changes
// keep the per-item protocol. See docs/GROUPED_APPLY.md for recovery ordering.
type applyFileGroup struct {
	parent     fsidentity.Identity
	checkpoint func(string, int) error
}

func planApplyFileGroup(destination *applyDestination, journal applyJournal, inputs map[string]applyPathInput, merged *treeAccess, checkpoint func(string, int) error) *applyFileGroup {
	if len(journal.Items) < 2 {
		return nil
	}
	parentPath := path.Dir(journal.Items[0].Path)
	parent := inputs[journal.Items[0].Path].ancestor.identity
	if parent.IsZero() {
		return nil
	}
	for _, item := range journal.Items {
		input := inputs[item.Path]
		if item.MetadataOnly || item.Original.Missing || item.Expected.Missing || path.Dir(item.Path) != parentPath || input.ancestor.path != parentPath || input.ancestor.identity != parent {
			return nil
		}
		original, err := destination.root.Lstat(item.Path)
		if err != nil || !original.Mode().IsRegular() {
			return nil
		}
		value, err := merged.root.Lstat(item.Path)
		// Physical mode must equal logical mode: grouped install skips mode
		// restoration, and grouped recovery hashes the still-staged physical value.
		if err != nil || !value.Mode().IsRegular() || value.Mode().Perm() != merged.original[item.Path] {
			return nil
		}
	}
	return &applyFileGroup{parent: parent, checkpoint: checkpoint}
}

func (g *applyFileGroup) check(event string, index int) error {
	if g.checkpoint != nil {
		return g.checkpoint(event, index)
	}
	return nil
}

func (g *applyFileGroup) install(destination *applyDestination, journal *applyJournal, synchronization applySynchronization) error {
	parent, err := openChangeParent(destination.root, journal.Items[0].Path, g.parent)
	if err != nil {
		return err
	}
	defer parent.Close()
	for i := range journal.Items {
		journal.Items[i].Parent = g.parent
		journal.Items[i].Phase = applyItemGrouped
	}
	if err := writeApplyJournalAtRoot(destination.root, *journal); err != nil {
		return err
	}
	if err := g.check("intent", -1); err != nil {
		return err
	}
	verify := func(item applyJournalItem) error {
		return errors.Join(destination.verifyPath(), verifyChangeParent(destination.root, item.Path, g.parent))
	}
	for i, item := range journal.Items {
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
	// Every item shares this parent; verify it once before publication.
	if err := verify(journal.Items[0]); err != nil {
		return err
	}
	if err := g.check("install-barrier", -1); err != nil {
		return err
	}
	if err := synchronization.barrier("install-parent", parent); err != nil {
		return err
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
