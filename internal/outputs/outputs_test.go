package outputs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
)

func TestCollectContextRefusesCanceledCollection(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "artifact"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, collected, err := CollectContext(ctx, workspace, t.TempDir(), []proto.OutputSpec{{
		Path: "artifact", Collect: proto.OutputCollectAlways,
	}}, true, 1<<20)
	if !errors.Is(err, context.Canceled) || collected {
		t.Fatalf("CollectContext() = collected %t, error %v", collected, err)
	}
}

func TestCaptureBaselinesContextRefusesCanceledCapture(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := CaptureBaselinesContext(ctx, root, []proto.OutputSpec{{Path: "artifact"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CaptureBaselinesContext() error = %v, want context.Canceled", err)
	}
}

func TestCaptureWorkspaceBaselinesHonorsLimits(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("large"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := CaptureWorkspaceBaselinesContext(
		context.Background(), root, []proto.OutputSpec{{Path: "artifact"}}, 4, MaxOutputEntries,
	)
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("CaptureWorkspaceBaselinesContext() error = %v, want ErrLimitExceeded", err)
	}
	if err := os.Mkdir(filepath.Join(root, "tree"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tree", "child"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = CaptureWorkspaceBaselinesContext(
		context.Background(), root, []proto.OutputSpec{{Path: "tree"}}, 1<<20, 1,
	)
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("entry-limited CaptureWorkspaceBaselinesContext() error = %v, want ErrLimitExceeded", err)
	}
}

func TestCollectRejectsNestedGitMetadata(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "dist", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "dist", ".git", "config"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := Collect(workspace, t.TempDir(), []proto.OutputSpec{{
		Path: "dist", Collect: proto.OutputCollectAlways,
	}}, true, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "Git metadata") {
		t.Fatalf("Collect() nested Git metadata error = %v", err)
	}
}

func TestValidateBundleRejectsNestedGitMetadata(t *testing.T) {
	bundle := proto.OutputBundle{
		V: BundleVersion, Paths: []string{"dist"},
		Manifest: proto.Manifest{Entries: []proto.ManifestEntry{
			{Path: "dist", Type: proto.EntryDir, Mode: 0o755},
			{Path: "dist/.git", Type: proto.EntryDir, Mode: 0o755},
		}},
	}
	if err := ValidateBundle(bundle); err == nil || !strings.Contains(err.Error(), "Git metadata") {
		t.Fatalf("ValidateBundle() nested Git metadata error = %v", err)
	}
}

func TestValidateBundleRejectsCaseCollidingManifestPaths(t *testing.T) {
	bundle := proto.OutputBundle{
		V: BundleVersion, Paths: []string{"dist"},
		Manifest: proto.Manifest{Entries: []proto.ManifestEntry{
			{Path: "dist", Type: proto.EntryDir, Mode: 0o755},
			{Path: "dist/A", Type: proto.EntryDir, Mode: 0o755},
			{Path: "dist/a", Type: proto.EntryDir, Mode: 0o755},
		}},
	}
	if err := ValidateBundle(bundle); err == nil || !strings.Contains(err.Error(), "case-insensitive") {
		t.Fatalf("ValidateBundle() case collision error = %v", err)
	}
}

func TestRenameNoReplacePreservesConcurrentDestination(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "value"), []byte("remote"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("concurrent"), 0o600); err != nil {
		t.Fatal(err)
	}
	fromDir, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer fromDir.Close()
	toDir, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer toDir.Close()

	err = renameNoReplace(fromDir, "value", toDir, "artifact")
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("renameNoReplace() error = %v, want fs.ErrExist", err)
	}
	for name, want := range map[string]string{"value": "remote", "artifact": "concurrent"} {
		got, readErr := os.ReadFile(filepath.Join(root, name))
		if readErr != nil || string(got) != want {
			t.Fatalf("%s after conflict = %q, %v; want %q", name, got, readErr, want)
		}
	}
}

func TestNormalizeSpecsDefaultsAndRejectsOverlap(t *testing.T) {
	got, err := NormalizeSpecs([]proto.OutputSpec{{Path: "dist/app"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Collect != proto.OutputCollectSuccess || got[0].Apply != proto.OutputApplyManual {
		t.Fatalf("NormalizeSpecs() = %+v", got)
	}
	for _, specs := range [][]proto.OutputSpec{
		{{Path: "../secret"}},
		{{Path: "/tmp/output"}},
		{{Path: ".git/config"}},
		{{Path: "nested/.git/hooks/post-checkout"}},
		{{Path: "nested/.Git/hooks/post-checkout"}},
		{{Path: "dist"}, {Path: "dist/app"}},
		{{Path: "a"}, {Path: "a-b"}, {Path: "a/b"}},
		{{Path: "Dist"}, {Path: "dist"}},
		{{Path: "Build"}, {Path: "build/app"}},
		{{Path: "dist", Collect: "sometimes"}},
		{{Path: "dist", Apply: "force"}},
	} {
		if _, err := NormalizeSpecs(specs); err == nil {
			t.Fatalf("NormalizeSpecs(%+v) succeeded", specs)
		}
	}
	tooMany := make([]proto.OutputSpec, MaxOutputPaths+1)
	for i := range tooMany {
		tooMany[i].Path = fmt.Sprintf("artifact-%06d", i)
	}
	if _, err := NormalizeSpecs(tooMany); err == nil || !strings.Contains(err.Error(), "output declarations") {
		t.Fatalf("NormalizeSpecs(%d paths) error = %v", len(tooMany), err)
	}
}

func TestRecoverApplicationRollsBackInterruptedInstall(t *testing.T) {
	local, bundle, baselines, staged := applyFixture(t, "old", "new")
	transaction := NewApplyTransaction()
	result, err := Apply(staged, local, bundle, baselines, nil, "test-owner", transaction)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := loadApplyJournal(local, result.Transaction)
	if err != nil {
		t.Fatal(err)
	}
	journal.Phase = applyPhasePrepared
	if err := writeApplyJournal(local, journal); err != nil {
		t.Fatal(err)
	}
	pending, err := RecoverApplication(local, transaction)
	if err != nil {
		t.Fatal(err)
	}
	if pending != nil {
		t.Fatalf("interrupted transaction returned pending commit: %+v", pending)
	}
	got, err := os.ReadFile(filepath.Join(local, "artifact"))
	if err != nil || string(got) != "old" {
		t.Fatalf("rolled-back artifact = %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(local, transaction)); !os.IsNotExist(err) {
		t.Fatalf("transaction survived rollback: %v", err)
	}
}

func TestApplyPublishesJournalBeforeStagingValues(t *testing.T) {
	staged := t.TempDir()
	artifact := filepath.Join(staged, "artifact")
	f, err := os.OpenFile(artifact, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(128 << 20); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := Collect(staged, t.TempDir(), []proto.OutputSpec{{
		Path: "artifact", Collect: proto.OutputCollectAlways,
	}}, true, 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	f, err = os.OpenFile(artifact, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{1}, 0); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	baselines, err := CaptureBaselines(local, []proto.OutputSpec{{Path: "artifact"}})
	if err != nil {
		t.Fatal(err)
	}
	transaction := NewApplyTransaction()
	finished := make(chan error, 1)
	go func() {
		_, err := Apply(staged, local, bundle, baselines, nil, "test-owner", transaction)
		finished <- err
	}()
	value := filepath.Join(local, transaction, "000000", "value")
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		if _, err := os.Lstat(value); err == nil {
			if _, err := os.Lstat(filepath.Join(local, transaction, applyJournalFile)); err != nil {
				t.Fatalf("staging began before the recovery journal was published: %v", err)
			}
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case err := <-finished:
			t.Fatalf("Apply() finished before its staging order could be observed: %v", err)
		case <-deadline.C:
			t.Fatal("timed out waiting for staged output value")
		case <-time.After(time.Millisecond):
		}
	}
	if err := <-finished; err == nil || !strings.Contains(err.Error(), "changed after download") {
		t.Fatalf("Apply() error = %v", err)
	}
}

func TestRecoverApplicationDiscardsPartiallyStagedValues(t *testing.T) {
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "artifact"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := captureBaseline(local, "artifact")
	if err != nil {
		t.Fatal(err)
	}
	transaction := NewApplyTransaction()
	transactionPath := filepath.Join(local, transaction)
	if err := os.Mkdir(transactionPath, 0o700); err != nil {
		t.Fatal(err)
	}
	transactionIdentity, _, err := fsidentity.Lstat(transactionPath)
	if err != nil {
		t.Fatal(err)
	}
	journal := applyJournal{
		Version: applyJournalVersion, Transaction: transaction, TransactionIdentity: transactionIdentity,
		Owner: "test-owner", BundleRoot: strings.Repeat("0", 64), Phase: applyPhasePrepared,
		Items: []applyJournalItem{{
			Path: "artifact", ItemDir: "000000", Original: original,
			Expected: Baseline{Path: "artifact", Digest: strings.Repeat("1", 64)}, Phase: applyItemPrepared,
		}},
	}
	if err := writeApplyJournal(local, journal); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(transactionPath, "000000"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transactionPath, "000000", "value"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	pending, err := RecoverApplication(local, transaction)
	if err != nil {
		t.Fatal(err)
	}
	if pending != nil {
		t.Fatalf("partially staged recovery returned pending commit: %+v", pending)
	}
	if got, err := os.ReadFile(filepath.Join(local, "artifact")); err != nil || string(got) != "original" {
		t.Fatalf("original output changed = %q, %v", got, err)
	}
	if _, err := os.Lstat(transactionPath); !os.IsNotExist(err) {
		t.Fatalf("partially staged transaction survived recovery: %v", err)
	}
}

func TestRecoverApplicationRemovesInterruptedJournalPublication(t *testing.T) {
	local := t.TempDir()
	transaction := NewApplyTransaction()
	transactionPath := filepath.Join(local, transaction)
	if err := os.Mkdir(transactionPath, 0o700); err != nil {
		t.Fatal(err)
	}
	tmpJournal := filepath.Join(transactionPath, ".journal-"+proto.NewULID())
	if err := os.WriteFile(tmpJournal, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}

	pending, err := RecoverApplication(local, transaction)
	if err != nil {
		t.Fatal(err)
	}
	if pending != nil {
		t.Fatalf("interrupted journal publication returned pending commit: %+v", pending)
	}
	if _, err := os.Lstat(transactionPath); !os.IsNotExist(err) {
		t.Fatalf("interrupted journal transaction survived recovery: %v", err)
	}
}

func TestRecoverApplicationRetainsJournalFreeTransactionWithRecoveryData(t *testing.T) {
	local := t.TempDir()
	transaction := NewApplyTransaction()
	transactionPath := filepath.Join(local, transaction)
	if err := os.MkdirAll(filepath.Join(transactionPath, "000000"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := RecoverApplication(local, transaction); err == nil || !strings.Contains(err.Error(), "unexpected recovery data") {
		t.Fatalf("RecoverApplication() error = %v", err)
	}
	if _, err := os.Lstat(transactionPath); err != nil {
		t.Fatalf("journal-free recovery data was not retained: %v", err)
	}
}

func TestRollbackResumesFromQuarantinedInstalledValue(t *testing.T) {
	root := t.TempDir()
	transaction := NewApplyTransaction()
	itemDir := filepath.Join(root, transaction, "000000")
	if err := os.MkdirAll(itemDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(itemDir, "previous"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(itemDir, "installed"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldBaseline, err := captureBaselineAt(root, path.Join(transaction, "000000", "previous"), "artifact")
	if err != nil {
		t.Fatal(err)
	}
	newBaseline, err := captureBaselineAt(root, path.Join(transaction, "000000", "installed"), "artifact")
	if err != nil {
		t.Fatal(err)
	}
	parentID := mustOutputParentIdentity(t, root, "artifact")
	journal := applyJournal{
		Version: applyJournalVersion, Transaction: transaction, Owner: "test-owner",
		BundleRoot: strings.Repeat("0", 64), Phase: applyPhasePrepared,
		Items: []applyJournalItem{{
			Path: "artifact", ItemDir: "000000", Original: oldBaseline,
			Expected: newBaseline, Parent: parentID, Phase: applyItemInstalled,
		}},
	}
	if err := rollbackApplyJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "artifact"))
	if err != nil || string(got) != "old" {
		t.Fatalf("restored artifact = %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(itemDir, "installed")); !os.IsNotExist(err) {
		t.Fatalf("quarantined installed value survived: %v", err)
	}
}

func TestRecoverApplicationCompletesCommittedInstall(t *testing.T) {
	local, bundle, baselines, staged := applyFixture(t, "old", "new")
	transaction := NewApplyTransaction()
	if _, err := Apply(staged, local, bundle, baselines, nil, "test-owner", transaction); err != nil {
		t.Fatal(err)
	}
	pending, err := RecoverApplication(local, transaction)
	if err != nil {
		t.Fatal(err)
	}
	if pending == nil || pending.Owner != "test-owner" || pending.BundleRoot != bundle.Manifest.RootHash() || len(pending.Paths) != 1 {
		t.Fatalf("pending committed transaction = %+v", pending)
	}
	if err := CommitApply(local, transaction); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverApplicationContextRefusesCanceledRecovery(t *testing.T) {
	local, bundle, baselines, staged := applyFixture(t, "old", "new")
	transaction := NewApplyTransaction()
	if _, err := Apply(staged, local, bundle, baselines, nil, "test-owner", transaction); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RecoverApplicationContext(ctx, local, transaction); !errors.Is(err, context.Canceled) {
		t.Fatalf("RecoverApplicationContext() error = %v, want context.Canceled", err)
	}
	if _, err := os.Lstat(filepath.Join(local, transaction)); err != nil {
		t.Fatalf("canceled recovery removed transaction: %v", err)
	}
}

func TestRecoverApplicationPreservesBackupWhenInstalledOutputChanged(t *testing.T) {
	local, bundle, baselines, staged := applyFixture(t, "old", "new")
	transaction := NewApplyTransaction()
	if _, err := Apply(staged, local, bundle, baselines, nil, "test-owner", transaction); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "artifact"), []byte("user edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverApplication(local, transaction); err == nil || !strings.Contains(err.Error(), "changed before state recovery") {
		t.Fatalf("recovery error = %v", err)
	}
	backup := filepath.Join(local, transaction, "000000", "previous")
	got, err := os.ReadFile(backup)
	if err != nil || string(got) != "old" {
		t.Fatalf("preserved backup = %q, %v", got, err)
	}
}

func TestApplyBackupValidationRestoresConcurrentEdit(t *testing.T) {
	local, bundle, baselines, staged := applyFixture(t, "old", "new")
	transaction := NewApplyTransaction()
	if _, err := Apply(staged, local, bundle, baselines, nil, "test-owner", transaction); err != nil {
		t.Fatal(err)
	}
	journal, err := loadApplyJournal(local, transaction)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(local, transaction, "000000", "previous")
	if err := os.WriteFile(backup, []byte("concurrent edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(local)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	validationErr := validateApplyBackups(root, journal)
	if validationErr == nil || !strings.Contains(validationErr.Error(), "changed while it was being replaced") {
		t.Fatalf("backup validation error = %v", validationErr)
	}
	if err := abortApply(local, journal, validationErr); err == nil {
		t.Fatal("abortApply succeeded despite the conflict")
	}
	got, err := os.ReadFile(filepath.Join(local, "artifact"))
	if err != nil || string(got) != "concurrent edit" {
		t.Fatalf("restored artifact = %q, %v", got, err)
	}
}

func TestMoveOriginalToBackupRejectsPathCreatedAfterPreflight(t *testing.T) {
	local := t.TempDir()
	if err := os.Mkdir(filepath.Join(local, "transaction"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(local, "transaction", "000000"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "artifact"), []byte("concurrent edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(local)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	journal := applyJournal{Transaction: "transaction"}
	item := applyJournalItem{
		Path: "artifact", ItemDir: "000000", Original: Baseline{Path: "artifact", Missing: true}, Phase: applyItemInstalling,
	}
	item.Parent = mustOutputParentIdentity(t, local, item.Path)
	journal.Items = []applyJournalItem{item}
	parentDir, err := openOutputParent(root, item.Path, item.Parent)
	if err != nil {
		t.Fatal(err)
	}
	err = moveOriginalToBackup(root, journal, item, parentDir)
	if closeErr := parentDir.Close(); err == nil {
		err = closeErr
	}
	if err == nil || !strings.Contains(err.Error(), "changed while it was being replaced") {
		t.Fatalf("moveOriginalToBackup error = %v", err)
	}
	if err := abortApply(local, journal, err); err == nil {
		t.Fatal("abortApply succeeded despite the conflict")
	}
	got, err := os.ReadFile(filepath.Join(local, "artifact"))
	if err != nil || string(got) != "concurrent edit" {
		t.Fatalf("concurrent artifact = %q, %v", got, err)
	}
}

func TestOpenOutputParentRejectsReplacementAfterValidation(t *testing.T) {
	rootPath := t.TempDir()
	parent := filepath.Join(rootPath, "nested")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	want := mustOutputParentIdentity(t, rootPath, "nested/artifact")
	if err := os.Rename(parent, filepath.Join(rootPath, "original-parent")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}

	dir, err := openOutputParent(root, "nested/artifact", want)
	if dir != nil {
		dir.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "replaced parent directory") {
		t.Fatalf("openOutputParent() error = %v", err)
	}
}

func TestPinnedOutputParentCannotRedirectInstallation(t *testing.T) {
	rootPath := t.TempDir()
	parent := filepath.Join(rootPath, "nested")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootPath, "transaction", "000000"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "transaction", "000000", "value"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	want := mustOutputParentIdentity(t, rootPath, "nested/artifact")
	destinationDir, err := openOutputParent(root, "nested/artifact", want)
	if err != nil {
		t.Fatal(err)
	}
	defer destinationDir.Close()
	originalParent := filepath.Join(rootPath, "original-parent")
	if err := os.Rename(parent, originalParent); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	item := applyJournalItem{Path: "nested/artifact", ItemDir: "000000"}
	if err := installPreparedValue(root, applyJournal{Transaction: "transaction"}, item, destinationDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(parent, "artifact")); !os.IsNotExist(err) {
		t.Fatalf("installation reached replacement parent: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(originalParent, "artifact"))
	if err != nil || string(got) != "new" {
		t.Fatalf("installation through pinned parent = %q, %v", got, err)
	}
	if err := verifyOutputParent(root, item.Path, want); err == nil {
		t.Fatal("parent replacement was not detected after installation")
	}
}

func TestPinnedOutputParentCannotRedirectRollback(t *testing.T) {
	rootPath := t.TempDir()
	parent := filepath.Join(rootPath, "nested")
	itemDir := filepath.Join(rootPath, "transaction", "000000")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(itemDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "artifact"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(itemDir, "previous"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	want := mustOutputParentIdentity(t, rootPath, "nested/artifact")
	destinationDir, err := openOutputParent(root, "nested/artifact", want)
	if err != nil {
		t.Fatal(err)
	}
	defer destinationDir.Close()
	originalParent := filepath.Join(rootPath, "original-parent")
	if err := os.Rename(parent, originalParent); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "artifact"), []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := captureBaselineAt(rootPath, "transaction/000000/previous", "nested/artifact")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := captureBaselineAt(rootPath, "original-parent/artifact", "nested/artifact")
	if err != nil {
		t.Fatal(err)
	}
	journal := applyJournal{Transaction: "transaction"}
	item := applyJournalItem{
		Path: "nested/artifact", ItemDir: "000000", Original: original, Expected: expected,
		Parent: want, Phase: applyItemInstalled,
	}
	if err := rollbackApplyItemAtRootContext(context.Background(), root, journal, item, destinationDir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(parent, "artifact"))
	if err != nil || string(got) != "unrelated" {
		t.Fatalf("replacement parent artifact = %q, %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(originalParent, "artifact"))
	if err != nil || string(got) != "old" {
		t.Fatalf("rollback through pinned parent = %q, %v", got, err)
	}
}

func TestPinnedWorkspaceRootRollsBackAfterPathReplacement(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "artifact"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := captureBaseline(workspace, "artifact")
	if err != nil {
		t.Fatal(err)
	}
	transaction := NewApplyTransaction()
	backupDir := filepath.Join(workspace, transaction, "000000")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(workspace, "artifact"), filepath.Join(backupDir, "previous")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "artifact"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	expected, err := captureBaseline(workspace, "artifact")
	if err != nil {
		t.Fatal(err)
	}
	destination, err := openApplyDestination(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	parentID, err := captureOutputParentIdentity(destination.root, "artifact")
	if err != nil {
		t.Fatal(err)
	}
	journal := applyJournal{
		Version: applyJournalVersion, Transaction: transaction, Owner: "test-owner",
		BundleRoot: strings.Repeat("0", 64), Phase: applyPhasePrepared,
		Items: []applyJournalItem{{
			Path: "artifact", ItemDir: "000000", Original: original, Expected: expected,
			Parent: parentID, Phase: applyItemInstalled,
		}},
	}
	journal.TransactionIdentity, _, err = fsidentity.Lstat(filepath.Join(workspace, transaction))
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "moved-workspace")
	if err := os.Rename(workspace, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "replacement"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	pathErr := destination.verifyPath()
	if pathErr == nil {
		t.Fatal("workspace path replacement was not detected")
	}
	if err := abortApplyAtRoot(destination.root, journal, pathErr); err != pathErr {
		t.Fatalf("pinned rollback error = %v, want %v", err, pathErr)
	}
	got, err := os.ReadFile(filepath.Join(moved, "artifact"))
	if err != nil || string(got) != "old" {
		t.Fatalf("rolled-back artifact = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(moved, transaction)); !os.IsNotExist(err) {
		t.Fatalf("pinned transaction survived rollback: %v", err)
	}
	got, err = os.ReadFile(filepath.Join(workspace, "replacement"))
	if err != nil || string(got) != "keep" {
		t.Fatalf("replacement workspace changed = %q, %v", got, err)
	}
}

func TestApplyToWorkspaceRefusesIdenticalReplacementRoot(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := Collect(remote, jobDir, []proto.OutputSpec{{
		Path: "artifact", Collect: proto.OutputCollectAlways,
	}}, true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "artifact"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselines, err := CaptureBaselines(workspace, []proto.OutputSpec{{Path: "artifact"}})
	if err != nil {
		t.Fatal(err)
	}
	workspaceID, _, err := fsidentity.Lstat(workspace)
	if err != nil {
		t.Fatal(err)
	}
	staged := t.TempDir()
	archiveFile, err := OpenArchive(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Extract(archiveFile, staged, bundle, 1<<20); err != nil {
		archiveFile.Close()
		t.Fatal(err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(workspace, filepath.Join(parent, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "artifact"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	transaction := NewApplyTransaction()
	_, err = ApplyToWorkspace(
		staged, workspace, bundle, baselines, nil, "test-owner", transaction, workspaceID,
	)
	if err == nil || !strings.Contains(err.Error(), "not the workspace") {
		t.Fatalf("ApplyToWorkspace() error = %v", err)
	}
	got, readErr := os.ReadFile(filepath.Join(workspace, "artifact"))
	if readErr != nil || string(got) != "old" {
		t.Fatalf("replacement workspace changed = %q, %v", got, readErr)
	}
	if _, statErr := os.Lstat(filepath.Join(workspace, transaction)); !os.IsNotExist(statErr) {
		t.Fatalf("replacement workspace contains transaction: %v", statErr)
	}
}

func TestIdentityBoundRecoveryAndMatchingRefuseReplacementRoot(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := Collect(remote, jobDir, []proto.OutputSpec{{
		Path: "artifact", Collect: proto.OutputCollectAlways,
	}}, true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "artifact"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselines, err := CaptureBaselines(workspace, []proto.OutputSpec{{Path: "artifact"}})
	if err != nil {
		t.Fatal(err)
	}
	workspaceID, _, err := fsidentity.Lstat(workspace)
	if err != nil {
		t.Fatal(err)
	}
	staged := t.TempDir()
	archiveFile, err := OpenArchive(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Extract(archiveFile, staged, bundle, 1<<20); err != nil {
		archiveFile.Close()
		t.Fatal(err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatal(err)
	}
	transaction := NewApplyTransaction()
	if _, err := ApplyToWorkspace(
		staged, workspace, bundle, baselines, nil, "test-owner", transaction, workspaceID,
	); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "original")
	if err := os.Rename(workspace, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "artifact"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := OutputPathMatchesWorkspace(workspace, workspaceID, bundle, "artifact"); err == nil || !strings.Contains(err.Error(), "not the workspace") {
		t.Fatalf("OutputPathMatchesWorkspace() error = %v", err)
	}
	if _, err := RecoverApplicationToWorkspaceContext(context.Background(), workspace, transaction, workspaceID); err == nil || !strings.Contains(err.Error(), "not the workspace") {
		t.Fatalf("RecoverApplicationToWorkspaceContext() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, transaction)); err != nil {
		t.Fatalf("original transaction was not preserved: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(workspace, "artifact"))
	if err != nil || string(got) != "new" {
		t.Fatalf("replacement workspace changed = %q, %v", got, err)
	}
}

func TestCaptureBaselineAtRootDoesNotFollowLeafSymlink(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "target"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "target", "payload"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(workspace, "link")); err != nil {
		t.Fatal(err)
	}
	want, err := captureBaseline(workspace, "link")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	got, err := captureBaselineAtRootContext(context.Background(), root, "link", "link")
	if err != nil {
		t.Fatal(err)
	}
	if !sameBaseline(want, got) {
		t.Fatalf("rooted symlink baseline = %+v, want %+v", got, want)
	}
}

func TestRecoverApplicationRemovesParentsCreatedForMissingOutput(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	outputPath := "nested/deep/artifact"
	if err := os.MkdirAll(filepath.Join(remote, "nested", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, filepath.FromSlash(outputPath)), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := Collect(remote, jobDir, []proto.OutputSpec{{Path: outputPath, Collect: proto.OutputCollectAlways}}, true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	baselines, err := CaptureBaselines(local, []proto.OutputSpec{{Path: outputPath}})
	if err != nil {
		t.Fatal(err)
	}
	staged := t.TempDir()
	archiveFile, err := OpenArchive(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Extract(archiveFile, staged, bundle, 1<<20); err != nil {
		archiveFile.Close()
		t.Fatal(err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatal(err)
	}
	transaction := NewApplyTransaction()
	if _, err := Apply(staged, local, bundle, baselines, nil, "test-owner", transaction); err != nil {
		t.Fatal(err)
	}
	journal, err := loadApplyJournal(local, transaction)
	if err != nil {
		t.Fatal(err)
	}
	journal.Phase = applyPhasePrepared
	if err := writeApplyJournal(local, journal); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverApplication(local, transaction); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(local, "nested")); !os.IsNotExist(err) {
		t.Fatalf("created output parents survived rollback: %v", err)
	}
}

func TestRollbackPreservesParentWhenInstallIntentDidNotCommit(t *testing.T) {
	root := t.TempDir()
	transaction := NewApplyTransaction()
	parentSource := filepath.Join(root, transaction, applyParentStagingDirectory, "000000")
	if err := os.MkdirAll(parentSource, 0o700); err != nil {
		t.Fatal(err)
	}
	parentIdentity, _, err := fsidentity.Lstat(parentSource)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	journal := applyJournal{
		Version: applyJournalVersion, Transaction: transaction, Owner: "test-owner",
		BundleRoot: strings.Repeat("0", 64), Phase: applyPhasePrepared,
		Items: []applyJournalItem{{
			Path: "nested/artifact", ItemDir: "000000",
			Original: Baseline{Path: "nested/artifact", Missing: true},
			Expected: Baseline{Path: "nested/artifact", Digest: strings.Repeat("1", 64)},
			Phase:    applyItemPrepared,
		}},
		CreatedParents: []applyJournalParent{{Path: "nested", Identity: parentIdentity}},
	}
	if err := rollbackApplyJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(root, "nested")); err != nil || !info.IsDir() {
		t.Fatalf("concurrently created parent was removed: %v, %v", info, err)
	}
}

func TestRollbackPreservesReplacedCreatedParent(t *testing.T) {
	rootPath := t.TempDir()
	transaction := NewApplyTransaction()
	if err := os.Mkdir(filepath.Join(rootPath, transaction), 0o700); err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(rootPath, "nested")
	if err := os.Mkdir(created, 0o755); err != nil {
		t.Fatal(err)
	}
	identity, _, err := fsidentity.Lstat(created)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(created, filepath.Join(rootPath, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(created, 0o700); err != nil {
		t.Fatal(err)
	}
	replacement, _, err := fsidentity.Lstat(created)
	if err != nil {
		t.Fatal(err)
	}
	journal := applyJournal{
		Version: applyJournalVersion, Transaction: transaction, Owner: "test-owner",
		BundleRoot: strings.Repeat("0", 64), Phase: applyPhasePrepared,
		Items: []applyJournalItem{{
			Path: "nested/artifact", ItemDir: "000000",
			Original: Baseline{Path: "nested/artifact", Missing: true},
			Expected: Baseline{Path: "nested/artifact", Digest: strings.Repeat("1", 64)},
			Phase:    applyItemPrepared,
		}},
		CreatedParents: []applyJournalParent{{Path: "nested", Identity: identity}},
	}
	if err := rollbackApplyJournal(rootPath, journal); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("rollback replacement error = %v", err)
	}
	got, _, err := fsidentity.Lstat(created)
	if err != nil {
		t.Fatalf("replacement parent was removed: %v", err)
	}
	if got != replacement {
		t.Fatalf("replacement parent identity = %+v, want %+v", got, replacement)
	}
}

func TestEnsureOutputParentsReportsOnlyDirectoriesItCreates(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	transaction := NewApplyTransaction()
	if err := os.Mkdir(filepath.Join(rootPath, transaction), 0o700); err != nil {
		t.Fatal(err)
	}
	journal := applyJournal{
		Version: applyJournalVersion, Transaction: transaction, Owner: "test-owner",
		BundleRoot: strings.Repeat("0", 64), Phase: applyPhasePrepared,
		Items: []applyJournalItem{{
			Path: "nested/deep/artifact", ItemDir: "000000",
			Original: Baseline{Path: "nested/deep/artifact", Missing: true},
			Expected: Baseline{Path: "nested/deep/artifact", Digest: strings.Repeat("1", 64)},
			Phase:    applyItemPrepared,
		}},
	}
	journal.TransactionIdentity, _, err = fsidentity.Lstat(filepath.Join(rootPath, transaction))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeApplyJournal(rootPath, journal); err != nil {
		t.Fatal(err)
	}

	if err := ensureOutputParents(root, &journal, "nested/deep/artifact"); err != nil {
		t.Fatal(err)
	}
	if len(journal.CreatedParents) != 1 || journal.CreatedParents[0].Path != "nested/deep" || journal.CreatedParents[0].Identity.IsZero() {
		t.Fatalf("ensureOutputParents() created = %v, want [nested/deep]", journal.CreatedParents)
	}
	if err := ensureOutputParents(root, &journal, "nested/deep/other"); err != nil {
		t.Fatal(err)
	}
	if len(journal.CreatedParents) != 1 {
		t.Fatalf("second ensureOutputParents() created = %v, want unchanged", journal.CreatedParents)
	}
}

func applyFixture(t *testing.T, localValue, remoteValue string) (string, proto.OutputBundle, []Baseline, string) {
	t.Helper()
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte(remoteValue), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := Collect(remote, jobDir, []proto.OutputSpec{{Path: "artifact", Collect: proto.OutputCollectAlways}}, true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "artifact"), []byte(localValue), 0o600); err != nil {
		t.Fatal(err)
	}
	baselines, err := CaptureBaselines(local, []proto.OutputSpec{{Path: "artifact"}})
	if err != nil {
		t.Fatal(err)
	}
	staged := t.TempDir()
	archiveFile, err := OpenArchive(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Extract(archiveFile, staged, bundle, 1<<20); err != nil {
		archiveFile.Close()
		t.Fatal(err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatal(err)
	}
	return local, bundle, baselines, staged
}

func mustOutputParentIdentity(t *testing.T, rootPath, outputPath string) fsidentity.Identity {
	t.Helper()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	identity, err := captureOutputParentIdentity(root, outputPath)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestCaptureBaselinesDistinguishesMissingAndContent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dist", "app"), []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	baselines, err := CaptureBaselines(root, []proto.OutputSpec{{Path: "dist/app"}, {Path: "report.json"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(baselines) != 2 || baselines[0].Digest == "" || baselines[0].Missing || !baselines[1].Missing {
		t.Fatalf("CaptureBaselines() = %+v", baselines)
	}
	if err := os.WriteFile(filepath.Join(root, "dist", "app"), []byte("changed"), 0o755); err != nil {
		t.Fatal(err)
	}
	current, err := CaptureBaselines(root, []proto.OutputSpec{{Path: "dist/app"}})
	if err != nil {
		t.Fatal(err)
	}
	if current[0].Digest == baselines[0].Digest {
		t.Fatal("content change did not change the baseline")
	}
}

func TestCollectCommitsSelectedOutputsAsImmutableBundle(t *testing.T) {
	workspace := t.TempDir()
	jobDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "dist", "app"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "failure.log"), []byte("trace"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := Collect(workspace, jobDir, []proto.OutputSpec{
		{Path: "dist", Collect: proto.OutputCollectSuccess, Apply: proto.OutputApplyAuto},
		{Path: "failure.log", Collect: proto.OutputCollectAlways, Apply: proto.OutputApplyManual},
	}, true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !collected || bundle.V != BundleVersion || bundle.Bytes != 11 || len(bundle.Paths) != 2 {
		t.Fatalf("Collect() = collected %t, bundle %+v", collected, bundle)
	}
	loaded, err := Load(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Manifest.RootHash() != bundle.Manifest.RootHash() {
		t.Fatalf("loaded bundle root = %s, want %s", loaded.Manifest.RootHash(), bundle.Manifest.RootHash())
	}
	archiveFile, err := OpenArchive(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	defer archiveFile.Close()
	if info, err := archiveFile.Stat(); err != nil || info.Size() == 0 {
		t.Fatalf("archive stat = %v, %v", info, err)
	}
}

func TestCollectHonorsSuccessConditionAndSizeLimit(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "success.bin"), []byte("success"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, collected, err := Collect(workspace, t.TempDir(), []proto.OutputSpec{
		{Path: "success.bin", Collect: proto.OutputCollectSuccess},
	}, false, 1<<20); err != nil || collected {
		t.Fatalf("failed-process collection = collected %t, err %v", collected, err)
	}
	jobDir := t.TempDir()
	if _, _, err := Collect(workspace, jobDir, []proto.OutputSpec{
		{Path: "success.bin", Collect: proto.OutputCollectAlways},
	}, false, 3); err == nil {
		t.Fatal("oversized output collection succeeded")
	}
	if _, err := os.Stat(filepath.Join(jobDir, BundleDirectory)); !os.IsNotExist(err) {
		t.Fatalf("failed collection committed output directory: %v", err)
	}
}

func TestExtractAndApplyRefusesChangedDestination(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := Collect(remote, jobDir, []proto.OutputSpec{{Path: "artifact", Collect: proto.OutputCollectAlways}}, true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "artifact"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselines, err := CaptureBaselines(local, []proto.OutputSpec{{Path: "artifact"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "artifact"), []byte("user edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	staged := t.TempDir()
	archiveFile, err := OpenArchive(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Extract(archiveFile, staged, bundle, 1<<20); err != nil {
		archiveFile.Close()
		t.Fatal(err)
	}
	archiveFile.Close()
	if _, err := Apply(staged, local, bundle, baselines, map[string]bool{"artifact": true}, "test-owner", NewApplyTransaction()); err == nil {
		t.Fatal("Apply succeeded over a changed destination")
	}
	got, err := os.ReadFile(filepath.Join(local, "artifact"))
	if err != nil || !bytes.Equal(got, []byte("user edit")) {
		t.Fatalf("destination after conflict = %q, %v", got, err)
	}
}

func TestExtractAndApplyReplacesMatchingDestination(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(remote, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "dist", "app"), []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := Collect(remote, jobDir, []proto.OutputSpec{{Path: "dist", Collect: proto.OutputCollectAlways}}, true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	if err := os.MkdirAll(filepath.Join(local, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "dist", "app"), []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	baselines, err := CaptureBaselines(local, []proto.OutputSpec{{Path: "dist"}})
	if err != nil {
		t.Fatal(err)
	}
	staged := t.TempDir()
	archiveFile, err := OpenArchive(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Extract(archiveFile, staged, bundle, 1<<20); err != nil {
		archiveFile.Close()
		t.Fatal(err)
	}
	archiveFile.Close()
	result, err := Apply(staged, local, bundle, baselines, map[string]bool{"dist": true}, "test-owner", NewApplyTransaction())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 1 || result.Applied[0] != "dist" {
		t.Fatalf("Apply result = %+v", result)
	}
	if err := CommitApply(local, result.Transaction); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(local, "dist", "app"))
	if err != nil || !bytes.Equal(got, []byte("new")) {
		t.Fatalf("applied output = %q, %v", got, err)
	}
	info, err := os.Stat(filepath.Join(local, "dist", "app"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("applied output mode = %v, %v", info, err)
	}
}

func TestApplyRefusesLocallyModifiedStaging(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("authentic"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := Collect(remote, jobDir, []proto.OutputSpec{{Path: "artifact", Collect: proto.OutputCollectAlways}}, true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	baselines, err := CaptureBaselines(local, []proto.OutputSpec{{Path: "artifact"}})
	if err != nil {
		t.Fatal(err)
	}
	staged := t.TempDir()
	archiveFile, err := OpenArchive(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Extract(archiveFile, staged, bundle, 1<<20); err != nil {
		archiveFile.Close()
		t.Fatal(err)
	}
	archiveFile.Close()
	if err := os.WriteFile(filepath.Join(staged, "artifact"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(staged, local, bundle, baselines, nil, "test-owner", NewApplyTransaction()); err == nil || !strings.Contains(err.Error(), "changed after download") {
		t.Fatalf("Apply tampered staging error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(local, "artifact")); !os.IsNotExist(err) {
		t.Fatalf("tampered output reached destination: %v", err)
	}
}

func TestCommitApplyRefusesReplacementTransactionDirectory(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := Collect(remote, jobDir, []proto.OutputSpec{{
		Path: "artifact", Collect: proto.OutputCollectAlways,
	}}, true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	baselines, err := CaptureBaselines(local, []proto.OutputSpec{{Path: "artifact"}})
	if err != nil {
		t.Fatal(err)
	}
	staged := t.TempDir()
	archiveFile, err := OpenArchive(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Extract(archiveFile, staged, bundle, 1<<20); err != nil {
		archiveFile.Close()
		t.Fatal(err)
	}
	archiveFile.Close()
	result, err := Apply(staged, local, bundle, baselines, nil, "test-owner", NewApplyTransaction())
	if err != nil {
		t.Fatal(err)
	}
	transaction := filepath.Join(local, result.Transaction)
	retained := transaction + "-original"
	if err := os.Rename(transaction, retained); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(transaction, 0o700); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(filepath.Join(retained, applyJournalFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transaction, applyJournalFile), journal, 0o600); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(transaction, "keep")
	if err := os.WriteFile(keep, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := CommitApply(local, result.Transaction); err == nil {
		t.Fatal("CommitApply removed a replacement transaction directory")
	}
	if got, err := os.ReadFile(keep); err != nil || string(got) != "replacement" {
		t.Fatalf("replacement transaction changed = %q, %v", got, err)
	}
}

func TestApplyCreatesMissingParentsInsideDestination(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	outputPath := "nested/deep/artifact"
	if err := os.MkdirAll(filepath.Join(remote, "nested", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, filepath.FromSlash(outputPath)), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := Collect(remote, jobDir, []proto.OutputSpec{{Path: outputPath, Collect: proto.OutputCollectAlways}}, true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	baselines, err := CaptureBaselines(local, []proto.OutputSpec{{Path: outputPath}})
	if err != nil {
		t.Fatal(err)
	}
	staged := t.TempDir()
	archiveFile, err := OpenArchive(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Extract(archiveFile, staged, bundle, 1<<20); err != nil {
		archiveFile.Close()
		t.Fatal(err)
	}
	archiveFile.Close()
	result, err := Apply(staged, local, bundle, baselines, nil, "test-owner", NewApplyTransaction())
	if err != nil {
		t.Fatal(err)
	}
	if err := CommitApply(local, result.Transaction); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(local, filepath.FromSlash(outputPath)))
	if err != nil || string(got) != "new" {
		t.Fatalf("nested applied output = %q, %v", got, err)
	}
}
