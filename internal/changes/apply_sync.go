package changes

import (
	"errors"
	"fmt"
	"os"
	"path"
)

type applySyncKind string

const (
	applyMemberSync  applySyncKind = "member"
	applyBarrierSync applySyncKind = "barrier"
)

// Observation is per application, never global. Tests see the actual operation
// selected below, so changing a publication barrier to a member sync is visible.
type applySynchronization struct {
	observe func(string, applySyncKind, *os.File) error
}

func (s applySynchronization) member(role string, file *os.File) error {
	if s.observe != nil {
		if err := s.observe(role, applyMemberSync, file); err != nil {
			return err
		}
	}
	return syncStagedData(file)
}

func (s applySynchronization) barrier(role string, file *os.File) error {
	if s.observe != nil {
		if err := s.observe(role, applyBarrierSync, file); err != nil {
			return err
		}
	}
	return syncStagingBarrier(file)
}

// A renamed directory entry does not establish persistence of dirty file data.
// Both installation strategies sync regular-file backups before publishing the
// rename. Directory-tree backup durability remains on the reference protocol.
func synchronizeApplyBackup(root *os.Root, journal applyJournal, item applyJournalItem, synchronization applySynchronization) error {
	backup := path.Join(journal.Transaction, item.ItemDir, "previous")
	before, err := root.Lstat(backup)
	if err != nil {
		return err
	}
	if before.Mode().IsRegular() {
		file, err := openSearchSourceFile(root, backup)
		if err != nil {
			return err
		}
		opened, err := file.Stat()
		if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
			return errors.Join(fmt.Errorf("backup %q changed while opening", item.Path), err, file.Close())
		}
		if err := errors.Join(synchronization.member("backup-data", file), file.Close()); err != nil {
			return err
		}
	}
	return validateApplyBackup(root, journal, item)
}
