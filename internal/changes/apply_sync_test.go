package changes

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
)

func TestApplySynchronizationOrder(t *testing.T) {
	for _, grouped := range []bool{false, true} {
		t.Run(fmt.Sprint(grouped), func(t *testing.T) {
			paths := []string{"a"}
			if grouped {
				paths = []string{"a", "b", "c"}
			}
			target, bundle, staged := groupFixturePaths(t, paths)
			var events []string
			_, err := target.Apply(staged, bundle, nil, ApplyOptions{syncCheckpoint: func(role string, kind applySyncKind, file *os.File) error {
				events = append(events, role+":"+string(kind))
				if role == "backup-data" {
					info, err := file.Stat()
					if err != nil || !info.Mode().IsRegular() {
						t.Fatalf("backup data descriptor: %v %v", info, err)
					}
					body := make([]byte, len("before-a"))
					if _, err := file.ReadAt(body, 0); err != nil || string(body[:7]) != "before-" {
						t.Fatalf("wrong synchronized body: %q %v", body, err)
					}
				}
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			if grouped {
				for range paths {
					want = append(want, "stage-member:member", "stage-member:member")
				}
				want = append(want, "stage-publication:barrier")
			}
			for range paths {
				member := "barrier"
				if grouped {
					member = "member"
				}
				want = append(want, "backup-data:member", "backup-directory:"+member, "backup-parent:barrier", "install-directory:"+member)
				if !grouped {
					want = append(want, "install-parent:barrier")
				}
			}
			if grouped {
				want = append(want, "install-parent:barrier")
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("sync order = %v; want %v", events, want)
			}
		})
	}
}

func TestBackupDataSyncFailurePreventsInstallation(t *testing.T) {
	for _, paths := range [][]string{{"a"}, {"a", "b", "c"}} {
		t.Run(fmt.Sprint(len(paths)), func(t *testing.T) {
			target, bundle, staged := groupFixturePaths(t, paths)
			injected := errors.New("backup data sync failed")
			_, err := target.Apply(staged, bundle, nil, ApplyOptions{syncCheckpoint: func(role string, _ applySyncKind, _ *os.File) error {
				if role == "backup-data" {
					return injected
				}
				if role == "install-directory" {
					t.Fatal("installed after failed backup sync")
				}
				return nil
			}})
			if !errors.Is(err, injected) {
				t.Fatal(err)
			}
			for _, name := range paths {
				assertTransferFile(t, target.Root, name, "before-"+name)
			}
		})
	}
}

func TestGroupedJournalRejectsMixedProtocols(t *testing.T) {
	for _, phase := range []string{applyItemPrepared, applyItemInstalling, applyItemInstalled} {
		t.Run(phase, func(t *testing.T) {
			target, bundle, staged := groupFixture(t)
			_, err := target.Apply(staged, bundle, nil, ApplyOptions{groupCheckpoint: func(event string, _ int) error {
				if event != "intent" {
					return nil
				}
				entries, err := os.ReadDir(target.Root)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range entries {
					if !validApplyTransaction(e.Name()) {
						continue
					}
					journal, err := loadApplyJournal(target.Root, e.Name())
					if err != nil {
						t.Fatal(err)
					}
					journal.Items[0].Phase = phase
					if err := validateApplyJournal(journal); err == nil {
						t.Fatal("accepted mixed grouped protocol")
					}
					return nil
				}
				t.Fatal("missing grouped intent")
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
