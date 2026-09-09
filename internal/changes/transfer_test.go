package changes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func transferTarget(t *testing.T, root string) TransferTarget {
	t.Helper()
	id, err := applyWorkspaceIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	return TransferTarget{Root: root, RootID: id, Owner: "transfer-owner", StatePath: filepath.Join(t.TempDir(), "apply.json")}
}

func TestTransferRetryPreservesSubsequentEdits(t *testing.T) {
	root, bundle, staged := applyFixture(t, "base\n", "incoming\n")
	target := transferTarget(t, root)
	first, err := target.Apply(staged, bundle, nil, ApplyOptions{})
	if err != nil || len(first.States) != 1 {
		t.Fatalf("apply = %+v, %v", first, err)
	}
	writeTransferFile(t, root, "artifact", "later edit\n")
	retry, err := target.Apply(staged, bundle, nil, ApplyOptions{})
	if err != nil || !reflect.DeepEqual(first, retry) {
		t.Fatalf("retry = %+v, %v; first = %+v", retry, err, first)
	}
	assertTransferFile(t, root, "artifact", "later edit\n")
	if pending, err := WorkspaceHasApplyTransactions(root); err != nil || pending {
		t.Fatalf("pending transaction after completion: %t, %v", pending, err)
	}
}

func TestTransferConflictReceiptDoesNotRemergeMarkers(t *testing.T) {
	root, bundle, staged := applyFixture(t, "base\n", "incoming\n")
	writeTransferFile(t, root, "artifact", "destination\n")
	target := transferTarget(t, root)
	opts := ApplyOptions{MaterializeConflicts: true}
	first, err := target.Apply(staged, bundle, nil, opts)
	var conflict *MergeConflictError
	if !errors.As(err, &conflict) || !conflict.Materialized || len(first.States) != 1 || len(first.Applied) != 0 {
		t.Fatalf("materialize = %+v, %v", first, err)
	}
	markers, err := os.ReadFile(filepath.Join(root, "artifact"))
	if err != nil || !strings.Contains(string(markers), "<<<<<<<") {
		t.Fatalf("markers = %q, %v", markers, err)
	}
	writeTransferFile(t, root, "artifact", "resolved\n")
	retry, err := target.Apply(staged, bundle, nil, opts)
	if !errors.As(err, &conflict) || !reflect.DeepEqual(first, retry) {
		t.Fatalf("retry = %+v, %v", retry, err)
	}
	assertTransferFile(t, root, "artifact", "resolved\n")
}

func TestTransferRefusalIsAnImmutableOutcome(t *testing.T) {
	root, bundle, staged := applyFixture(t, "base\n", "incoming\n")
	writeTransferFile(t, root, "artifact", "destination\n")
	target := transferTarget(t, root)
	_, err := target.Apply(staged, bundle, nil, ApplyOptions{})
	var conflict *MergeConflictError
	if !errors.As(err, &conflict) || conflict.Materialized {
		t.Fatalf("refusal = %v", err)
	}
	assertTransferFile(t, root, "artifact", "destination\n")
	writeTransferFile(t, root, "artifact", "base\n")
	if _, err := target.Apply(staged, bundle, nil, ApplyOptions{}); !errors.As(err, &conflict) {
		t.Fatalf("retry should replay refusal: %v", err)
	}
	assertTransferFile(t, root, "artifact", "base\n")
	// A new application is a new intent and may merge the same staged bundle.
	if _, err := transferTarget(t, root).Apply(staged, bundle, nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	assertTransferFile(t, root, "artifact", "incoming\n")
}

func TestTransferRecoveryPreservesPartialConflictOutcome(t *testing.T) {
	for _, boundary := range []string{"before-install", "before-receipt", "before-cleanup", "after-cleanup"} {
		t.Run(boundary, func(t *testing.T) {
			root, bundle, staged := mixedTransferFixture(t)
			target := transferTarget(t, root)
			state := transferApplyState{Version: 1, Owner: target.Owner, RootID: target.RootID, BundleRoot: bundle.RootHash(), Paths: bundle.Paths, Materialize: true, Pending: NewApplyTransaction()}
			persistTransferState(t, target, state)
			if boundary != "before-install" {
				installed, err := ApplyToWorkspace(staged, root, bundle, nil, target.Owner, state.Pending, target.RootID, ApplyOptions{MaterializeConflicts: true})
				if err != nil {
					t.Fatal(err)
				}
				if boundary != "before-receipt" {
					state.Outcome = &transferOutcome{Applied: installed.Applied, Conflicts: installed.Conflicts, States: installed.States}
					persistTransferState(t, target, state)
				}
				if boundary == "after-cleanup" {
					if err := CommitApplyToWorkspace(root, state.Pending, target.RootID); err != nil {
						t.Fatal(err)
					}
				}
				if boundary == "before-cleanup" || boundary == "after-cleanup" {
					for _, path := range bundle.Paths {
						writeTransferFile(t, root, path, "subsequent work\n")
					}
				}
			}
			result, err := target.Apply(staged, bundle, nil, ApplyOptions{MaterializeConflicts: true})
			var conflict *MergeConflictError
			if !errors.As(err, &conflict) || !conflict.Materialized || !reflect.DeepEqual(result.Applied, []string{"clean"}) || !reflect.DeepEqual(result.Conflicts, []string{"binary", "text"}) {
				t.Fatalf("recovery = %+v, %v", result, err)
			}
			if boundary == "before-cleanup" || boundary == "after-cleanup" {
				for _, path := range bundle.Paths {
					assertTransferFile(t, root, path, "subsequent work\n")
				}
			} else {
				assertTransferFile(t, root, "binary", "destination\x00")
				assertTransferFile(t, root, "clean", "incoming\n")
				markers, _ := os.ReadFile(filepath.Join(root, "text"))
				if !strings.Contains(string(markers), "<<<<<<<") || !strings.Contains(string(markers), "incoming") {
					t.Fatalf("missing conflict contents: %q", markers)
				}
			}
			for _, path := range bundle.Paths {
				writeTransferFile(t, root, path, "subsequent work\n")
			}
			retry, err := target.Apply(staged, bundle, nil, ApplyOptions{MaterializeConflicts: true})
			if !errors.As(err, &conflict) || !reflect.DeepEqual(result, retry) {
				t.Fatalf("retry = %+v, %v", retry, err)
			}
			for _, path := range bundle.Paths {
				assertTransferFile(t, root, path, "subsequent work\n")
			}
			if pending, err := WorkspaceHasApplyTransactions(root); err != nil || pending {
				t.Fatalf("pending journal: %t, %v", pending, err)
			}
		})
	}
}

func TestTransferRecoveryRefusesEditsBeforeReceipt(t *testing.T) {
	root, bundle, staged := applyFixture(t, "base\n", "incoming\n")
	target := transferTarget(t, root)
	state := transferApplyState{Version: 1, Owner: target.Owner, RootID: target.RootID, BundleRoot: bundle.RootHash(), Paths: bundle.Paths, Pending: NewApplyTransaction()}
	persistTransferState(t, target, state)
	if _, err := ApplyToWorkspace(staged, root, bundle, nil, target.Owner, state.Pending, target.RootID, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	writeTransferFile(t, root, "artifact", "concurrent edit\n")
	if _, err := target.Apply(staged, bundle, nil, ApplyOptions{}); err == nil {
		t.Fatal("recovery accepted changed installed content")
	}
	assertTransferFile(t, root, "artifact", "concurrent edit\n")
	if pending, err := WorkspaceHasApplyTransactions(root); err != nil || !pending {
		t.Fatalf("recovery must retain backup: %t, %v", pending, err)
	}
}

func TestTransferChecksJournalOwnerBeforeRollback(t *testing.T) {
	root, bundle, staged := applyFixture(t, "base\n", "incoming\n")
	target := transferTarget(t, root)
	state := transferApplyState{Version: 1, Owner: target.Owner, RootID: target.RootID, BundleRoot: bundle.RootHash(), Paths: bundle.Paths, Pending: NewApplyTransaction()}
	persistTransferState(t, target, state)
	if _, err := ApplyToWorkspace(staged, root, bundle, nil, "another-owner", state.Pending, target.RootID, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	journal, err := loadApplyJournal(root, state.Pending)
	if err != nil {
		t.Fatal(err)
	}
	journal.Phase = applyPhasePrepared
	if err := writeApplyJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Apply(staged, bundle, nil, ApplyOptions{}); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("foreign journal should be refused before rollback: %v", err)
	}
	assertTransferFile(t, root, "artifact", "incoming\n")
}

func TestTransferSelectionAndRequestIdentity(t *testing.T) {
	root, bundle, staged := mixedTransferFixture(t)
	target := transferTarget(t, root)
	selection := map[string]bool{"clean": true}
	if _, err := target.Apply(staged, bundle, selection, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	assertTransferFile(t, root, "clean", "incoming\n")
	assertTransferFile(t, root, "text", "destination\n")
	assertTransferFile(t, root, "binary", "destination\x00")
	for _, changed := range []string{"selection", "options", "owner", "bundle", "workspace"} {
		t.Run(changed, func(t *testing.T) {
			other, b, paths, opts := target, bundle, selection, ApplyOptions{}
			switch changed {
			case "selection":
				paths = nil
			case "options":
				opts.MaterializeConflicts = true
			case "owner":
				other.Owner = "different-owner"
			case "bundle":
				_, b, _ = applyFixture(t, "other\n", "new\n")
				paths = nil
			case "workspace":
				other.Root = t.TempDir()
				other.RootID, _ = applyWorkspaceIdentity(other.Root)
			}
			if _, err := other.Apply(staged, b, paths, opts); err == nil || !strings.Contains(err.Error(), "does not match its recorded request") {
				t.Fatalf("expected receipt identity refusal, got %v", err)
			}
		})
	}
}

func TestTransferStateMustRemainOutsideWorkspace(t *testing.T) {
	for _, location := range []string{"root", "descendant", "symlink"} {
		t.Run(location, func(t *testing.T) {
			root, bundle, staged := applyFixture(t, "base\n", "incoming\n")
			parent := root
			if location != "root" {
				parent = filepath.Join(root, "receipts")
				if err := os.Mkdir(parent, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if location == "symlink" {
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(parent, alias); err != nil {
					t.Fatal(err)
				}
				parent = alias
			}
			target := transferTarget(t, root)
			target.StatePath = filepath.Join(parent, "receipt.json")
			if _, err := target.Apply(staged, bundle, nil, ApplyOptions{}); err == nil {
				t.Fatal("accepted state inside files being merged")
			}
			assertTransferFile(t, root, "artifact", "base\n")
		})
	}
}

func TestTransferEmptySelectionAndInvalidOutcome(t *testing.T) {
	root, bundle, staged := applyFixture(t, "base\n", "incoming\n")
	target := transferTarget(t, root)
	result, err := target.Apply(staged, bundle, map[string]bool{}, ApplyOptions{})
	if err != nil || len(result.States) != 0 || len(result.Applied) != 0 {
		t.Fatalf("empty selection = %+v, %v", result, err)
	}
	assertTransferFile(t, root, "artifact", "base\n")
	if _, err := target.Apply(staged, bundle, map[string]bool{}, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	// A malformed completion must not turn an unapplied request into success.
	state := transferApplyState{Version: 1, Owner: target.Owner, RootID: target.RootID, BundleRoot: bundle.RootHash(), Paths: bundle.Paths, Outcome: &transferOutcome{}}
	persistTransferState(t, target, state)
	if _, err := target.Apply(staged, bundle, nil, ApplyOptions{}); err == nil {
		t.Fatal("accepted an outcome missing its selected paths")
	}
	assertTransferFile(t, root, "artifact", "base\n")
}

func TestTransferRecoveryRollsBackUncommittedInstallation(t *testing.T) {
	root, bundle, staged := applyFixture(t, "base\n", "incoming\n")
	target := transferTarget(t, root)
	state := transferApplyState{Version: 1, Owner: target.Owner, RootID: target.RootID, BundleRoot: bundle.RootHash(), Paths: bundle.Paths, Pending: NewApplyTransaction()}
	persistTransferState(t, target, state)
	if _, err := ApplyToWorkspace(staged, root, bundle, nil, target.Owner, state.Pending, target.RootID, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	journal, err := loadApplyJournal(root, state.Pending)
	if err != nil {
		t.Fatal(err)
	}
	// The file is installed but the committed journal write did not happen.
	journal.Phase = applyPhasePrepared
	if err := writeApplyJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	result, err := target.Apply(staged, bundle, nil, ApplyOptions{})
	if err != nil || !reflect.DeepEqual(result.Applied, []string{"artifact"}) {
		t.Fatalf("retry after rollback = %+v, %v", result, err)
	}
	assertTransferFile(t, root, "artifact", "incoming\n")
	if pending, err := WorkspaceHasApplyTransactions(root); err != nil || pending {
		t.Fatalf("pending transaction after retry: %t, %v", pending, err)
	}
}

func persistTransferState(t *testing.T, target TransferTarget, state transferApplyState) {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(target.StatePath))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := writeTransferState(root, filepath.Base(target.StatePath), state); err != nil {
		t.Fatal(err)
	}
}

func mixedTransferFixture(t *testing.T) (string, proto.ChangeBundle, string) {
	t.Helper()
	remote, local, job := t.TempDir(), t.TempDir(), t.TempDir()
	paths := []string{"binary", "clean", "text"}
	for _, path := range paths {
		writeTransferFile(t, remote, path, "base\n")
		writeTransferFile(t, local, path, "base\n")
	}
	manifest, err := snapshot.Build(remote, paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := CaptureWorkspaceBaseContext(context.Background(), remote, job, manifest); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		writeTransferFile(t, remote, path, "incoming\n")
	}
	writeTransferFile(t, remote, "binary", "incoming\x00")
	writeTransferFile(t, local, "binary", "destination\x00")
	writeTransferFile(t, local, "text", "destination\n")
	bundle, _, err := CollectWorkspaceChangesContext(context.Background(), remote, job, manifest, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return local, bundle, extractTestBundle(t, job, bundle)
}

func writeTransferFile(t *testing.T, root, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func assertTransferFile(t *testing.T, root, name, value string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(root, name))
	if err != nil || string(got) != value {
		t.Fatalf("%s = %q, %v; want %q", name, got, err, value)
	}
}
