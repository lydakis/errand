package changes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTransferInterruptedCleanup(t *testing.T) {
	for _, removed := range []string{"backup", "journal", "transaction", "replaced-transaction"} {
		t.Run(removed, func(t *testing.T) {
			root, bundle, staged := applyFixture(t, "base\n", "incoming\n")
			target := transferTarget(t, root)
			state := transferApplyState{Version: 1, Owner: target.Owner, RootID: target.RootID, BundleRoot: bundle.RootHash(), Paths: bundle.Paths, Pending: NewApplyTransaction()}
			installed, err := ApplyToWorkspace(staged, root, bundle, nil, target.Owner, state.Pending, target.RootID, ApplyOptions{})
			if err != nil {
				t.Fatal(err)
			}
			state.Outcome = &transferOutcome{Applied: installed.Applied, Conflicts: installed.Conflicts, States: installed.States}
			persistTransferState(t, target, state)
			destination, err := openApplyDestinationWithIdentity(root, target.RootID)
			if err != nil {
				t.Fatal(err)
			}
			defer destination.Close()
			// Interrupt just after publishing cleanup intent, before any deletion.
			interrupted := errors.New("interrupted cleanup")
			transaction := state.Pending
			err = target.finishTransfer(destination, &state, func() error {
				persistTransferState(t, target, state)
				return interrupted
			})
			if !errors.Is(err, interrupted) {
				t.Fatal(err)
			}
			transactionPath := filepath.Join(root, transaction)
			if _, err := os.Stat(transactionPath); err != nil {
				t.Fatalf("cleanup must be recorded before deleting recovery data: %v", err)
			}
			// These are possible states after interruption during recursive removal.
			switch removed {
			case "backup":
				err = os.Remove(filepath.Join(transactionPath, "000000", "previous"))
			case "journal":
				err = os.Remove(filepath.Join(transactionPath, applyJournalFile))
			case "transaction":
				err = os.RemoveAll(transactionPath)
			case "replaced-transaction":
				if err = os.Rename(transactionPath, filepath.Join(t.TempDir(), "original")); err == nil {
					err = os.Mkdir(transactionPath, 0700)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			writeTransferFile(t, root, "artifact", "later edit\n")
			for attempt := 0; attempt < 2; attempt++ {
				result, err := target.Apply(staged, bundle, nil, ApplyOptions{})
				if removed == "replaced-transaction" {
					if err == nil {
						t.Fatal("cleanup accepted a replaced transaction")
					}
					if _, err := os.Stat(transactionPath); err != nil {
						t.Fatalf("replacement removed: %v", err)
					}
				} else if err != nil || !reflect.DeepEqual(result.States, installed.States) || !reflect.DeepEqual(result.Applied, installed.Applied) {
					t.Fatalf("retry %d = %+v, %v", attempt, result, err)
				}
				assertTransferFile(t, root, "artifact", "later edit\n")
			}
			if removed != "replaced-transaction" {
				if pending, err := WorkspaceHasApplyTransactions(root); err != nil || pending {
					t.Fatalf("pending transaction after cleanup: %t, %v", pending, err)
				}
			}
		})
	}
}

func TestTransferMaterializationFailureRemainsRetryable(t *testing.T) {
	root, bundle, staged := applyFixture(t, "base\n", "incoming\n")
	target := transferTarget(t, root)
	state := transferApplyState{Version: 1, Owner: target.Owner, RootID: target.RootID, BundleRoot: bundle.RootHash(), Paths: bundle.Paths, Materialize: true}
	destination, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	// The same error escapes installation if a metadata directory becomes a file
	// after the merge was prepared. Exercise that error boundary without a race.
	_, _, _, conflictErr := captureMetadataBaselineAtRoot(context.Background(), destination, "artifact", "artifact")
	var conflict *MergeConflictError
	if !errors.As(conflictErr, &conflict) {
		t.Fatalf("expected metadata conflict, got %v", conflictErr)
	}
	applyErr := fmt.Errorf("checking local change: %w", conflictErr)
	if err := state.recordRefusal(applyErr); !errors.Is(err, applyErr) || state.Outcome != nil {
		t.Fatalf("materialization failure became a completed outcome: %+v, %v", state.Outcome, err)
	}
	persistTransferState(t, target, state)
	if _, err := target.Apply(staged, bundle, nil, ApplyOptions{MaterializeConflicts: true}); err != nil {
		t.Fatal(err)
	}
	assertTransferFile(t, root, "artifact", "incoming\n")
}

func TestTransferCompletedCleanupRetainsUntrustedRecoveryData(t *testing.T) {
	for _, altered := range []string{"backup", "owner", "phase", "missing-backup", "missing-journal"} {
		t.Run(altered, func(t *testing.T) {
			root, bundle, staged := applyFixture(t, "base\n", "incoming\n")
			target := transferTarget(t, root)
			state := transferApplyState{Version: 1, Owner: target.Owner, RootID: target.RootID, BundleRoot: bundle.RootHash(), Paths: bundle.Paths, Pending: NewApplyTransaction()}
			installed, err := ApplyToWorkspace(staged, root, bundle, nil, target.Owner, state.Pending, target.RootID, ApplyOptions{})
			if err != nil {
				t.Fatal(err)
			}
			state.Outcome = &transferOutcome{Applied: installed.Applied, Conflicts: installed.Conflicts, States: installed.States}
			persistTransferState(t, target, state)
			journal, err := loadApplyJournal(root, state.Pending)
			if err != nil {
				t.Fatal(err)
			}
			switch altered {
			case "backup":
				writeTransferFile(t, root, filepath.Join(state.Pending, "000000", "previous"), "altered backup\n")
			case "owner":
				journal.Owner = "another-owner"
			case "phase":
				journal.Phase = applyPhasePrepared
			}
			if err := writeApplyJournal(root, journal); err != nil {
				t.Fatal(err)
			}
			if altered == "missing-backup" || altered == "missing-journal" {
				name := filepath.Join(state.Pending, "000000", "previous")
				if altered == "missing-journal" {
					name = filepath.Join(state.Pending, applyJournalFile)
				}
				if err := os.Remove(filepath.Join(root, name)); err != nil {
					t.Fatal(err)
				}
			}
			writeTransferFile(t, root, "artifact", "later edit\n")
			if _, err := target.Apply(staged, bundle, nil, ApplyOptions{}); err == nil {
				t.Fatal("removed untrusted recovery data")
			}
			assertTransferFile(t, root, "artifact", "later edit\n")
			if _, err := os.Stat(filepath.Join(root, state.Pending)); err != nil {
				t.Fatalf("recovery data removed: %v", err)
			}
			if altered == "backup" {
				assertTransferFile(t, root, filepath.Join(state.Pending, "000000", "previous"), "altered backup\n")
			}
		})
	}
}

func TestTransferStateRejectsCaseAliasedAncestors(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "descendant"}[nested], func(t *testing.T) {
			original, bundle, staged := applyFixture(t, "base\n", "incoming\n")
			parent := t.TempDir()
			root := filepath.Join(parent, "Work")
			if err := os.Rename(original, root); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(parent, "work")
			if _, err := os.Stat(alias); os.IsNotExist(err) {
				t.Skip("case-sensitive filesystem")
			} else if err != nil {
				t.Fatal(err)
			}
			if nested {
				alias = filepath.Join(alias, "receipts")
				if err := os.Mkdir(alias, 0700); err != nil {
					t.Fatal(err)
				}
			}
			target := transferTarget(t, root)
			target.StatePath = filepath.Join(alias, "apply.json")
			if _, err := target.Apply(staged, bundle, nil, ApplyOptions{}); err == nil {
				t.Fatal("accepted receipt inside workspace through case alias")
			}
			assertTransferFile(t, root, "artifact", "base\n")
			if _, err := os.Lstat(target.StatePath); !os.IsNotExist(err) {
				t.Fatalf("receipt created in workspace: %v", err)
			}
		})
	}
}
