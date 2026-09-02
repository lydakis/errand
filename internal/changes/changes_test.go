package changes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
	"golang.org/x/sys/unix"
)

type testChangeRoot struct {
	Path string
}

func captureTestBaselines(root string, specs []testChangeRoot) ([]Baseline, error) {
	baselines := make([]Baseline, 0, len(specs))
	for _, spec := range specs {
		baseline, err := captureBaseline(root, spec.Path)
		if err != nil {
			return nil, err
		}
		baselines = append(baselines, baseline)
	}
	return baselines, nil
}

func collectTestChanges(workspace, jobDir string, specs []testChangeRoot, maxBytes int64) (proto.ChangeBundle, bool, error) {
	paths := make([]string, 0)
	for _, spec := range specs {
		err := filepath.WalkDir(filepath.Join(workspace, filepath.FromSlash(spec.Path)), func(current string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(workspace, current)
			if err != nil {
				return err
			}
			paths = append(paths, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return proto.ChangeBundle{}, false, err
		}
	}
	manifest, err := snapshot.Build(workspace, paths)
	if err != nil {
		return proto.ChangeBundle{}, false, err
	}
	bundle := proto.ChangeBundle{V: BundleVersion, BaselineRoot: (proto.Manifest{}).RootHash(), RemoteManifest: manifest}
	for _, spec := range specs {
		bundle.Paths = append(bundle.Paths, spec.Path)
	}
	sort.Strings(bundle.Paths)
	for _, entry := range manifest.Entries {
		if entry.Type != proto.EntryFile {
			continue
		}
		if maxBytes >= 0 && entry.Size > maxBytes-bundle.Bytes {
			return proto.ChangeBundle{}, false, ErrLimitExceeded
		}
		bundle.Bytes += entry.Size
	}
	if err := commitBundle(workspace, workspace, jobDir, bundle); err != nil {
		return proto.ChangeBundle{}, false, err
	}
	return bundle, true, nil
}

func extractTestBundle(t *testing.T, jobDir string, bundle proto.ChangeBundle) string {
	t.Helper()
	staged := t.TempDir()
	base := filepath.Join(staged, "base")
	remote := filepath.Join(staged, "remote")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(remote, 0o700); err != nil {
		t.Fatal(err)
	}
	baseArchive, err := OpenBaseArchive(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ExtractBase(baseArchive, base, bundle, 1<<30); err != nil {
		baseArchive.Close()
		t.Fatal(err)
	}
	if err := baseArchive.Close(); err != nil {
		t.Fatal(err)
	}
	remoteArchive, err := OpenRemoteArchive(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ExtractRemote(remoteArchive, remote, bundle, 1<<30); err != nil {
		remoteArchive.Close()
		t.Fatal(err)
	}
	if err := remoteArchive.Close(); err != nil {
		t.Fatal(err)
	}
	return staged
}

func TestCollectWorkspaceChangesCapturesCreatedModifiedAndDeletedPaths(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "changed"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "deleted"), []byte("gone"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselinePaths := []string{"changed", "deleted"}
	baseline, err := snapshot.Build(workspace, baselinePaths)
	if err != nil {
		t.Fatal(err)
	}
	jobDir := t.TempDir()
	if err := CaptureWorkspaceBaseContext(context.Background(), workspace, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "changed"), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(workspace, "deleted")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "created", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "created", "nested", "value"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := CollectWorkspaceChangesContext(
		context.Background(), workspace, jobDir, baseline, proto.SelectionPolicy{}, 1<<20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !collected {
		t.Fatal("CollectWorkspaceChangesContext() did not retain changes")
	}
	wantPaths := []string{"changed", "created", "deleted"}
	if fmt.Sprint(bundle.Paths) != fmt.Sprint(wantPaths) {
		t.Fatalf("paths = %v, want %v", bundle.Paths, wantPaths)
	}
	if bundle.BaselineRoot != baseline.RootHash() {
		t.Fatalf("baseline root = %q, want %q", bundle.BaselineRoot, baseline.RootHash())
	}
	if got := subtreeManifest(bundle.BaseManifest, "created"); len(got.Entries) != 0 {
		t.Fatalf("created path has submitted entries: %+v", got.Entries)
	}
	for _, name := range []string{"changed", "deleted"} {
		if got := subtreeManifest(bundle.BaseManifest, name); len(got.Entries) == 0 {
			t.Fatalf("submitted path %q is missing", name)
		}
	}
	if got := subtreeManifest(bundle.RemoteManifest, "deleted"); len(got.Entries) != 0 {
		t.Fatalf("deleted path has final entries: %+v", got.Entries)
	}
}

func TestCollectWorkspaceChangesUsesFrozenPolicyForNewPaths(t *testing.T) {
	workspace := t.TempDir()
	for name, contents := range map[string]string{
		".errandignore": "target/\n",
		"tracked":       "before",
	} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	baseline, err := snapshot.Build(workspace, []string{".errandignore", "tracked"})
	if err != nil {
		t.Fatal(err)
	}
	jobDir := t.TempDir()
	if err := CaptureWorkspaceBaseContext(context.Background(), workspace, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "tracked"), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "generated"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, "target"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "target", "large"), bytes.Repeat([]byte("x"), 2<<20), 0o600); err != nil {
		t.Fatal(err)
	}

	policy := proto.SelectionPolicy{Ignore: []string{"target/"}}
	bundle, collected, err := CollectWorkspaceChangesContext(
		context.Background(), workspace, jobDir, baseline, policy, 1<<20,
	)
	if err != nil || !collected {
		t.Fatalf("collection = %+v, %t, %v", bundle, collected, err)
	}
	if got, want := fmt.Sprint(bundle.Paths), "[.errandignore generated tracked]"; got != want {
		t.Fatalf("paths = %s, want %s", got, want)
	}
	for _, entry := range bundle.RemoteManifest.Entries {
		if entry.Path == "target" || strings.HasPrefix(entry.Path, "target/") {
			t.Fatalf("frozen policy retained ignored path %+v", entry)
		}
	}
}

func TestCollectWorkspaceChangesAlwaysComparesSubmittedIgnoredPath(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "target"), 0o700); err != nil {
		t.Fatal(err)
	}
	tracked := filepath.Join(workspace, "target", "submitted")
	if err := os.WriteFile(tracked, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, err := snapshot.Build(workspace, []string{"target/submitted"})
	if err != nil {
		t.Fatal(err)
	}
	jobDir := t.TempDir()
	if err := CaptureWorkspaceBaseContext(context.Background(), workspace, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracked, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := CollectWorkspaceChangesContext(
		context.Background(), workspace, jobDir, baseline,
		proto.SelectionPolicy{Ignore: []string{"target/"}}, 1<<20,
	)
	if err != nil || !collected {
		t.Fatalf("collection = %+v, %t, %v", bundle, collected, err)
	}
	if got, want := fmt.Sprint(bundle.Paths), "[target/submitted]"; got != want {
		t.Fatalf("paths = %s, want %s", got, want)
	}
}

func TestCollectWorkspaceChangesUsesGitQuestionMarkPolicy(t *testing.T) {
	workspace := t.TempDir()
	if out, err := exec.Command("git", "-C", workspace, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte("foo?\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "keep"), []byte("submitted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "foo1"), []byte("already ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, _, policy, err := snapshot.SelectFiles(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(paths, "foo1") {
		t.Fatalf("Git selected question-mark ignored path: %v", paths)
	}
	baseline, err := snapshot.Build(workspace, paths)
	if err != nil {
		t.Fatal(err)
	}
	jobDir := t.TempDir()
	if err := CaptureWorkspaceBaseContext(context.Background(), workspace, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "fooa"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := CollectWorkspaceChangesContext(
		context.Background(), workspace, jobDir, baseline, policy, 1<<20,
	)
	if err != nil || collected || len(bundle.Paths) != 0 {
		t.Fatalf("ignored wildcard change was retained: %+v, %t, %v", bundle, collected, err)
	}
}

func TestCollectWorkspaceChangesKeepsPathsOutsideNestedGitignoreScope(t *testing.T) {
	workspace := t.TempDir()
	if out, err := exec.Command("git", "-C", workspace, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "nested", ".gitignore"), []byte("foo/bar\nsecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, _, policy, err := snapshot.SelectFiles(workspace)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := snapshot.Build(workspace, paths)
	if err != nil {
		t.Fatal(err)
	}
	jobDir := t.TempDir()
	if err := CaptureWorkspaceBaseContext(context.Background(), workspace, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	for name := range map[string]bool{
		"other/nested/foo/bar": true,
		"other/nested/secret":  true,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(workspace, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("retained"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bundle, collected, err := CollectWorkspaceChangesContext(
		context.Background(), workspace, jobDir, baseline, policy, 1<<20,
	)
	if err != nil || !collected {
		t.Fatalf("collection = %+v, %t, %v", bundle, collected, err)
	}
	if got, want := fmt.Sprint(bundle.Paths), "[other]"; got != want {
		t.Fatalf("paths = %s, want %s", got, want)
	}
}

func TestCollectWorkspaceChangesWithEmptyBaselineCapturesGeneratedTree(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "generated", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "generated", "nested", "value"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := CollectWorkspaceChangesContext(
		context.Background(), workspace, t.TempDir(), proto.Manifest{}, proto.SelectionPolicy{}, 1<<20,
	)
	if err != nil || !collected {
		t.Fatalf("CollectWorkspaceChangesContext() = collected %t, error %v", collected, err)
	}
	if fmt.Sprint(bundle.Paths) != fmt.Sprint([]string{"generated"}) {
		t.Fatalf("paths = %v, want [generated]", bundle.Paths)
	}
}

func TestCollectWorkspaceChangesKeepsLexicalSiblingsAsSeparateRoots(t *testing.T) {
	remote := t.TempDir()
	if err := os.MkdirAll(filepath.Join(remote, "build", "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "build", "out", "artifact"), []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "build.log"), []byte("log"), 0o600); err != nil {
		t.Fatal(err)
	}
	jobDir := t.TempDir()
	bundle, collected, err := CollectWorkspaceChangesContext(
		context.Background(), remote, jobDir, proto.Manifest{}, proto.SelectionPolicy{}, 1<<20,
	)
	if err != nil || !collected {
		t.Fatalf("collection = %+v, %t, %v", bundle, collected, err)
	}
	if got, want := fmt.Sprint(bundle.Paths), fmt.Sprint([]string{"build", "build.log"}); got != want {
		t.Fatalf("paths = %s, want %s", got, want)
	}
	staged := extractTestBundle(t, jobDir, bundle)
	local := t.TempDir()
	result, err := Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction())
	if err != nil {
		t.Fatal(err)
	}
	if err := CommitApply(local, result.Transaction); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"build/out/artifact": "artifact", "build.log": "log"} {
		got, err := os.ReadFile(filepath.Join(local, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Fatalf("applied %s = %q, %v", name, got, err)
		}
	}
}

func TestRecoverApplicationRollsBackMetadataOnlyChangeInPlace(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "tree")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	identity, _, err := fsidentity.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	transaction := NewApplyTransaction()
	if err := os.MkdirAll(filepath.Join(root, transaction, "000000"), 0o700); err != nil {
		t.Fatal(err)
	}
	transactionIdentity, _, err := fsidentity.Lstat(filepath.Join(root, transaction))
	if err != nil {
		t.Fatal(err)
	}
	journal := applyJournal{
		Version: applyJournalVersion, Transaction: transaction, TransactionIdentity: transactionIdentity,
		Owner: "test-owner", BundleRoot: strings.Repeat("0", 64), Phase: applyPhasePrepared,
		Items: []applyJournalItem{{
			Path: "tree", ItemDir: "000000", Original: metadataBaseline("tree", 0o755),
			Expected: metadataBaseline("tree", 0o700), Parent: mustChangeParentIdentity(t, root, "tree"),
			Phase: applyItemInstalled, MetadataOnly: true, Target: identity,
			OriginalMode: 0o755, ExpectedMode: 0o700,
		}},
	}
	if err := writeApplyJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	if pending, err := RecoverApplication(root, transaction); err != nil || pending != nil {
		t.Fatalf("RecoverApplication() = %+v, %v", pending, err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("rolled-back directory mode = %v, %v", info, err)
	} else if current, _, err := fsidentity.Lstat(dir); err != nil || current != identity {
		t.Fatalf("rolled-back directory identity = %+v, %v", current, err)
	}
}

func TestApplyRecreatesUntrackedParentForCreatedRoot(t *testing.T) {
	remote := t.TempDir()
	if err := os.Mkdir(filepath.Join(remote, "build"), 0o700); err != nil {
		t.Fatal(err)
	}
	baseline, err := snapshot.Build(remote, []string{"build"})
	if err != nil {
		t.Fatal(err)
	}
	jobDir := t.TempDir()
	if err := CaptureWorkspaceBaseContext(context.Background(), remote, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "build", "artifact"), []byte("remote"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := CollectWorkspaceChangesContext(
		context.Background(), remote, jobDir, baseline, proto.SelectionPolicy{}, 1<<20,
	)
	if err != nil || !collected {
		t.Fatalf("collection = %+v, %t, %v", bundle, collected, err)
	}
	if got, want := fmt.Sprint(bundle.Paths), fmt.Sprint([]string{"build/artifact"}); got != want {
		t.Fatalf("paths = %s, want %s", got, want)
	}
	staged := extractTestBundle(t, jobDir, bundle)
	matching := t.TempDir()
	if err := os.Mkdir(filepath.Join(matching, "build"), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(staged, matching, bundle, nil, "test-owner", NewApplyTransaction())
	if err != nil {
		t.Fatalf("apply with matching parent: %v", err)
	}
	if err := CommitApply(matching, result.Transaction); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(matching, "build", "artifact")); err != nil || string(got) != "remote" {
		t.Fatalf("applied artifact = %q, %v", got, err)
	}
	local := t.TempDir()
	if err := os.Mkdir(filepath.Join(local, "build"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(local, "build")); err != nil {
		t.Fatal(err)
	}
	_, err = Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction())
	var conflict *MergeConflictError
	if !errors.As(err, &conflict) || fmt.Sprint(conflict.Paths) != "[build]" {
		t.Fatalf("apply with deleted submitted parent = %v, want build conflict", err)
	}
	if _, err := os.Lstat(filepath.Join(local, "build")); !os.IsNotExist(err) {
		t.Fatalf("deleted local parent was recreated: %v", err)
	}
}

func TestApplyAllowsCreatedSiblingRootsInSeparateTransactions(t *testing.T) {
	remote := t.TempDir()
	if err := os.Mkdir(filepath.Join(remote, "build"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "build", "keep"), []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, err := snapshot.Build(remote, []string{"build", "build/keep"})
	if err != nil {
		t.Fatal(err)
	}
	jobDir := t.TempDir()
	if err := CaptureWorkspaceBaseContext(context.Background(), remote, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{"a": "first", "b": "second"} {
		if err := os.WriteFile(filepath.Join(remote, "build", name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bundle, collected, err := CollectWorkspaceChangesContext(
		context.Background(), remote, jobDir, baseline, proto.SelectionPolicy{}, 1<<20,
	)
	if err != nil || !collected {
		t.Fatalf("collection = %+v, %t, %v", bundle, collected, err)
	}
	if got, want := fmt.Sprint(bundle.Paths), fmt.Sprint([]string{"build/a", "build/b"}); got != want {
		t.Fatalf("paths = %s, want %s", got, want)
	}

	staged := extractTestBundle(t, jobDir, bundle)
	local := t.TempDir()
	if err := os.Mkdir(filepath.Join(local, "build"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "build", "keep"), []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"build/a", "build/b"} {
		result, err := Apply(
			staged, local, bundle, map[string]bool{name: true}, "test-owner", NewApplyTransaction(),
		)
		if err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if err := CommitApply(local, result.Transaction); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]string{"build/a": "first", "build/b": "second"} {
		got, err := os.ReadFile(filepath.Join(local, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Fatalf("applied %s = %q, %v", name, got, err)
		}
	}

	conflicting := t.TempDir()
	if err := os.Mkdir(filepath.Join(conflicting, "build"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conflicting, "build", "keep"), []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conflicting, "build", "local"), []byte("edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(
		staged, conflicting, bundle, map[string]bool{"build/a": true}, "test-owner", NewApplyTransaction(),
	)
	if err != nil {
		t.Fatalf("apply with unrelated parent edit: %v", err)
	}
	if err := CommitApply(conflicting, result.Transaction); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(conflicting, "build", "local")); err != nil || string(got) != "edit" {
		t.Fatalf("unrelated parent edit = %q, %v", got, err)
	}
}

func TestDeletedWorkspaceChangeAppliesOnlyAgainstOriginalValue(t *testing.T) {
	remote := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, err := snapshot.Build(remote, []string{"artifact"})
	if err != nil {
		t.Fatal(err)
	}
	jobDir := t.TempDir()
	if err := CaptureWorkspaceBaseContext(context.Background(), remote, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(remote, "artifact")); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := CollectWorkspaceChangesContext(context.Background(), remote, jobDir, baseline, proto.SelectionPolicy{}, 1<<20)
	if err != nil || !collected {
		t.Fatalf("collection = %+v, %t, %v", bundle, collected, err)
	}
	staged := extractTestBundle(t, jobDir, bundle)
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "artifact"), []byte("local edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction()); err == nil ||
		!strings.Contains(err.Error(), "artifact") {
		t.Fatalf("conflicting deletion error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(local, "artifact"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(local, "artifact")); !os.IsNotExist(err) {
		t.Fatalf("deleted artifact still exists: %v", err)
	}
	if err := CommitApply(local, result.Transaction); err != nil {
		t.Fatal(err)
	}
}

func TestCollectWorkspaceChangesContextRefusesCanceledCollection(t *testing.T) {
	workspace := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, collected, err := CollectWorkspaceChangesContext(ctx, workspace, t.TempDir(), proto.Manifest{}, proto.SelectionPolicy{}, 1<<20)
	if !errors.Is(err, context.Canceled) || collected {
		t.Fatalf("CollectWorkspaceChangesContext() = collected %t, error %v", collected, err)
	}
}

func TestCaptureBaselinesContextRefusesCanceledCapture(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := captureBaselineAtContext(ctx, root, "artifact", "artifact")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("captureTestBaselinesContext() error = %v, want context.Canceled", err)
	}
}

func TestCaptureWorkspaceBaselinesHonorsLimits(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("large"), 0o600); err != nil {
		t.Fatal(err)
	}
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rootFS.Close()
	_, _, _, err = captureBaselineAtRootBoundedContext(
		context.Background(), rootFS, "artifact", "artifact", 4, MaxChangeEntries,
	)
	if !errors.Is(err, ErrByteLimitExceeded) {
		t.Fatalf("CaptureWorkspaceBaselinesContext() error = %v, want ErrByteLimitExceeded", err)
	}
	if err := os.Mkdir(filepath.Join(root, "tree"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tree", "child"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = captureBaselineAtRootBoundedContext(
		context.Background(), rootFS, "tree", "tree", 1<<20, 1,
	)
	if !errors.Is(err, ErrEntryLimitExceeded) {
		t.Fatalf("entry-limited CaptureWorkspaceBaselinesContext() error = %v, want ErrEntryLimitExceeded", err)
	}
}

func TestCopyContextStopsBeforeReadingAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelAfterFirstRead{cancel: cancel}
	var dest bytes.Buffer
	if err := copyContext(ctx, &dest, reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("copyContext() error = %v, want context.Canceled", err)
	}
	if reader.readAfterCancel {
		t.Fatal("copyContext() read from the source after cancellation")
	}
}

type cancelAfterFirstRead struct {
	cancel          context.CancelFunc
	read            bool
	readAfterCancel bool
}

func (r *cancelAfterFirstRead) Read(p []byte) (int, error) {
	if r.read {
		r.readAfterCancel = true
		return 0, errors.New("read after cancellation")
	}
	r.read = true
	p[0] = 'x'
	r.cancel()
	return 1, nil
}

func TestWorkspacePathDiscoveryHonorsLimits(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "first"), []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := workspacePathsContext(context.Background(), root, 10, 3); !errors.Is(err, ErrByteLimitExceeded) {
		t.Fatalf("byte-limited discovery error = %v, want ErrByteLimitExceeded", err)
	}
	if err := os.WriteFile(filepath.Join(root, "second"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := workspacePathsContext(context.Background(), root, 1, 1<<20); !errors.Is(err, ErrEntryLimitExceeded) {
		t.Fatalf("entry-limited discovery error = %v, want ErrEntryLimitExceeded", err)
	}
}

func TestWorkspacePathDiscoveryKeepsOrdinaryTransactionPrefixedPaths(t *testing.T) {
	root := t.TempDir()
	ordinary := ".errand-change-not-a-transaction"
	if err := os.Mkdir(filepath.Join(root, ordinary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ordinary, "kept"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := workspacePathsContext(context.Background(), root, 10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range paths {
		if candidate == ordinary+"/kept" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("workspacePathsContext() = %v, want ordinary prefixed path", paths)
	}
}

func TestWorkspacePathDiscoveryExcludesApplyTransactions(t *testing.T) {
	root := t.TempDir()
	transaction := applyTransactionPrefix + proto.NewULID()
	if err := os.Mkdir(filepath.Join(root, transaction), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, transaction, "journal.json"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := workspacePathsContext(context.Background(), root, 10, 1<<20)
	if err != nil || fmt.Sprint(paths) != "[artifact]" {
		t.Fatalf("workspacePathsContext() = %v, %v; want [artifact]", paths, err)
	}
}

func TestCollectRefusesUnsupportedNodeReplacingSubmittedPath(t *testing.T) {
	workspace := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "artifact"), []byte("submitted"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, err := snapshot.Build(workspace, []string{"artifact"})
	if err != nil {
		t.Fatal(err)
	}
	if err := CaptureWorkspaceBaseContext(context.Background(), workspace, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(workspace, "artifact")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(workspace, "artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CollectWorkspaceChangesContext(context.Background(), workspace, jobDir, baseline, proto.SelectionPolicy{}, 1<<20); err == nil ||
		!strings.Contains(err.Error(), "replaces submitted path") {
		t.Fatalf("CollectWorkspaceChangesContext() error = %v", err)
	}
}

func TestCollectIgnoresNewUnsupportedNodesWithoutInventingDeletes(t *testing.T) {
	workspace := t.TempDir()
	jobDir := t.TempDir()
	baseline := proto.Manifest{}
	if err := CaptureWorkspaceBaseContext(context.Background(), workspace, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(workspace, "events"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "artifact"), []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := CollectWorkspaceChangesContext(context.Background(), workspace, jobDir, baseline, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !collected || fmt.Sprint(bundle.Paths) != "[artifact]" {
		t.Fatalf("collected = %t, paths = %v", collected, bundle.Paths)
	}
}

func TestCollectRetainsRestrictiveFinalModes(t *testing.T) {
	workspace := t.TempDir()
	jobDir := t.TempDir()
	baseline := proto.Manifest{}
	if err := CaptureWorkspaceBaseContext(context.Background(), workspace, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	sealed := filepath.Join(workspace, "sealed")
	defer os.Chmod(sealed, 0o700)
	if err := os.Mkdir(sealed, 0o700); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(sealed, "artifact")
	if err := os.WriteFile(artifact, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(artifact, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealed, 0); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := CollectWorkspaceChangesContext(context.Background(), workspace, jobDir, baseline, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !collected || fmt.Sprint(bundle.Paths) != "[sealed]" {
		t.Fatalf("collected = %t, paths = %v", collected, bundle.Paths)
	}
	for _, entry := range bundle.RemoteManifest.Entries {
		if (entry.Path == "sealed" || entry.Path == "sealed/artifact") && entry.Mode != 0 {
			t.Fatalf("remote mode for %s = %#o", entry.Path, entry.Mode)
		}
	}
	if info, err := os.Lstat(sealed); err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("workspace directory mode = %v, %v", info, err)
	}
	staged := extractTestBundle(t, jobDir, bundle)
	defer os.Chmod(filepath.Join(staged, "remote", "sealed"), 0o700)
	if err := VerifyExtracted(staged, bundle); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Join(staged, "remote", "sealed")); err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("staged directory mode = %v, %v", info, err)
	}
}

func TestApplyRetainedRestrictiveTree(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	baseline := proto.Manifest{}
	if err := CaptureWorkspaceBaseContext(context.Background(), remote, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	sealed := filepath.Join(remote, "sealed")
	if err := os.Mkdir(sealed, 0o700); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(sealed, "artifact")
	if err := os.WriteFile(artifact, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(artifact, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealed, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(sealed, 0o700)

	bundle, collected, err := CollectWorkspaceChangesContext(
		context.Background(), remote, jobDir, baseline, proto.SelectionPolicy{}, 1<<20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !collected {
		t.Fatal("restrictive workspace change was not collected")
	}
	staged := extractTestBundle(t, jobDir, bundle)
	defer os.Chmod(filepath.Join(staged, "remote", "sealed"), 0o700)
	local := t.TempDir()
	result, err := Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction())
	if err != nil {
		t.Fatal(err)
	}
	pending, err := RecoverApplication(local, result.Transaction)
	if err != nil {
		t.Fatal(err)
	}
	if pending == nil || fmt.Sprint(pending.Paths) != "[sealed]" {
		t.Fatalf("recovered application = %+v", pending)
	}
	if err := CommitApply(local, result.Transaction); err != nil {
		t.Fatal(err)
	}
	localSealed := filepath.Join(local, "sealed")
	localArtifact := filepath.Join(localSealed, "artifact")
	if info, err := os.Lstat(localSealed); err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("applied directory mode = %v, %v", info, err)
	}
	if err := os.Chmod(localSealed, 0o700); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(localSealed, 0o700)
	if info, err := os.Lstat(localArtifact); err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("applied file mode = %v, %v", info, err)
	}
	if err := os.Chmod(localArtifact, 0o400); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(localArtifact)
	if err != nil || string(got) != "retained" {
		t.Fatalf("applied file = %q, %v", got, err)
	}
}

func TestDirectoryModeOnlyChangeDoesNotRetainContents(t *testing.T) {
	workspace := t.TempDir()
	jobDir := t.TempDir()
	dir := filepath.Join(workspace, "tree")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "large"), bytes.Repeat([]byte("x"), 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, err := snapshot.Build(workspace, []string{"tree", "tree/large"})
	if err != nil {
		t.Fatal(err)
	}
	if err := CaptureWorkspaceBaseContext(context.Background(), workspace, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := CollectWorkspaceChangesContext(context.Background(), workspace, jobDir, baseline, proto.SelectionPolicy{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !collected || fmt.Sprint(bundle.Paths) != "[tree]" || fmt.Sprint(bundle.MetadataPaths) != "[tree]" || bundle.Bytes != 0 {
		t.Fatalf("mode-only bundle = collected %t, %+v", collected, bundle)
	}
	local := t.TempDir()
	localDir := filepath.Join(local, "tree")
	if err := os.Mkdir(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "large"), bytes.Repeat([]byte("x"), 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	localGit := filepath.Join(localDir, ".git")
	if err := os.Mkdir(localGit, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localGit, "HEAD"), []byte("local-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	localDirBefore, err := os.Stat(localDir)
	if err != nil {
		t.Fatal(err)
	}
	staged := extractTestBundle(t, jobDir, bundle)
	result, err := Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction())
	if err != nil {
		t.Fatal(err)
	}
	state := result.States["tree"]
	if !strings.HasPrefix(state, metadataStatePrefix) {
		t.Fatalf("metadata-only applied state = %q", state)
	}
	localIdentity, err := applyWorkspaceIdentity(local)
	if err != nil {
		t.Fatal(err)
	}
	if matches, err := ChangePathStateMatchesWorkspace(local, localIdentity, "tree", state); err != nil || !matches {
		t.Fatalf("metadata-only applied state matches = %t, %v", matches, err)
	}
	if err := CommitApply(local, result.Transaction); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(localDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("applied directory mode = %v, %v", info, err)
	} else if !os.SameFile(localDirBefore, info) {
		t.Fatal("metadata-only apply replaced the local directory")
	}
	if got, err := os.ReadFile(filepath.Join(localDir, "large")); err != nil || len(got) != 1024 {
		t.Fatalf("preserved child = %d bytes, %v", len(got), err)
	}
	if got, err := os.ReadFile(filepath.Join(localGit, "HEAD")); err != nil || string(got) != "local-only\n" {
		t.Fatalf("preserved nested Git metadata = %q, %v", got, err)
	}
}

func TestDirectoryModeAndChildChangeStayCompactAndApply(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	dir := filepath.Join(remote, "tree")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "changed"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "large"), bytes.Repeat([]byte("x"), 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, err := snapshot.Build(remote, []string{"tree", "tree/changed", "tree/large"})
	if err != nil {
		t.Fatal(err)
	}
	if err := CaptureWorkspaceBaseContext(context.Background(), remote, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "changed"), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := CollectWorkspaceChangesContext(
		context.Background(), remote, jobDir, baseline, proto.SelectionPolicy{}, 64,
	)
	if err != nil || !collected {
		t.Fatalf("collection = %+v, %t, %v", bundle, collected, err)
	}
	if got, want := fmt.Sprint(bundle.Paths), "[tree tree/changed]"; got != want {
		t.Fatalf("paths = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(bundle.MetadataPaths), "[tree]"; got != want {
		t.Fatalf("metadata paths = %s, want %s", got, want)
	}
	if bundle.Bytes != int64(len("before")+len("after")) {
		t.Fatalf("bundle bytes = %d", bundle.Bytes)
	}
	for _, entry := range append(bundle.BaseManifest.Entries, bundle.RemoteManifest.Entries...) {
		if entry.Path == "tree/large" {
			t.Fatal("unchanged sibling was retained")
		}
	}

	local := t.TempDir()
	localDir := filepath.Join(local, "tree")
	if err := os.Mkdir(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "changed"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "large"), bytes.Repeat([]byte("x"), 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	staged := extractTestBundle(t, jobDir, bundle)
	result, err := Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction())
	if err != nil {
		t.Fatal(err)
	}
	if err := CommitApply(local, result.Transaction); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(localDir, "changed")); err != nil || string(got) != "after" {
		t.Fatalf("changed child = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(localDir, "large")); err != nil || len(got) != 1024 {
		t.Fatalf("unchanged child = %d bytes, %v", len(got), err)
	}
	if info, err := os.Stat(localDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v, %v", info, err)
	}
}

func TestPublishBundleRemovesDestinationWhenParentSyncFails(t *testing.T) {
	parent := t.TempDir()
	tmp := filepath.Join(parent, ".changes-temp")
	dest := filepath.Join(parent, BundleDirectory)
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	err := publishBundleDirectory(tmp, dest, parent, func(string) error {
		calls++
		if calls == 1 {
			return errors.New("injected sync failure")
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "injected sync failure") {
		t.Fatalf("publishBundleDirectory() error = %v", err)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("uncommitted bundle survived failed publication: %v", err)
	}
}

func TestApplyRefusesUnreadableWorkspaceWithoutChangingMode(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(remote, "sealed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "sealed", "artifact"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, err := snapshot.Build(remote, []string{"sealed", "sealed/artifact"})
	if err != nil {
		t.Fatal(err)
	}
	if err := CaptureWorkspaceBaseContext(context.Background(), remote, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "sealed", "artifact"), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := CollectWorkspaceChangesContext(context.Background(), remote, jobDir, baseline, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	localSealed := filepath.Join(local, "sealed")
	if err := os.Mkdir(localSealed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localSealed, "artifact"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(localSealed, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(localSealed, 0o700)
	staged := extractTestBundle(t, jobDir, bundle)
	if _, err := Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction()); err == nil {
		t.Fatal("Apply unexpectedly read an inaccessible workspace")
	}
	if info, err := os.Lstat(localSealed); err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("workspace mode changed = %v, %v", info, err)
	}
}

func TestMakeTreeAccessibleContextHonorsEntryLimitAndRestoresModes(t *testing.T) {
	root := t.TempDir()
	sealed := filepath.Join(root, "sealed")
	if err := os.Mkdir(sealed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sealed, "artifact"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealed, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(sealed, 0o700)

	if _, err := makeTreeAccessibleContext(context.Background(), root, 1, -1); !errors.Is(err, ErrEntryLimitExceeded) {
		t.Fatalf("makeTreeAccessibleContext() error = %v, want ErrEntryLimitExceeded", err)
	}
	if info, err := os.Lstat(sealed); err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("sealed directory mode after bounded traversal = %v, %v", info, err)
	}
}

func TestCollectWorkspaceChangesHandlesInaccessibleWorkspaceRoot(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "artifact"), []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspace, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(workspace, 0o700)

	bundle, collected, err := CollectWorkspaceChangesContext(
		context.Background(), workspace, t.TempDir(), proto.Manifest{}, proto.SelectionPolicy{}, 1<<20,
	)
	if err != nil || !collected {
		t.Fatalf("collection = %+v, %t, %v", bundle, collected, err)
	}
	if got, want := fmt.Sprint(bundle.Paths), "[artifact]"; got != want {
		t.Fatalf("paths = %s, want %s", got, want)
	}
	if info, err := os.Lstat(workspace); err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("workspace root mode after collection = %v, %v", info, err)
	}
}

func TestMakeTreeAccessibleRestoresInaccessibleRootAfterFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0o700)

	if _, err := makeTreeAccessibleContext(context.Background(), root, 0, -1); !errors.Is(err, ErrEntryLimitExceeded) {
		t.Fatalf("makeTreeAccessibleContext() error = %v, want ErrEntryLimitExceeded", err)
	}
	if info, err := os.Lstat(root); err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("workspace root mode after failed traversal = %v, %v", info, err)
	}
}

func TestMakeManifestAccessibleContextTouchesOnlySelectedPathsAndHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	selected := filepath.Join(root, "selected")
	unselected := filepath.Join(root, "unselected")
	for _, dir := range []string{selected, unselected} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "artifact"), []byte("value"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := snapshot.Build(root, []string{"selected", "selected/artifact"})
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{selected, unselected} {
		if err := os.Chmod(dir, 0); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0o700)
	}

	access, err := makeManifestAccessibleContext(context.Background(), root, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := access.original["selected"]; !ok {
		t.Fatal("selected directory was not prepared")
	}
	if _, ok := access.original["unselected"]; ok {
		t.Fatal("unselected directory was prepared")
	}
	if err := access.restore(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{selected, unselected} {
		if info, err := os.Lstat(dir); err != nil || info.Mode().Perm() != 0 {
			t.Fatalf("directory %q mode after restore = %v, %v", dir, info, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := makeManifestAccessibleContext(ctx, root, manifest); !errors.Is(err, context.Canceled) {
		t.Fatalf("makeManifestAccessibleContext() error = %v, want context.Canceled", err)
	}
}

func TestWorkspaceDeltaHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := workspaceDelta(ctx, proto.Manifest{}, proto.Manifest{}, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("workspaceDelta() error = %v, want context.Canceled", err)
	}
}

func TestWorkspaceDeltaHandlesManyIndependentRoots(t *testing.T) {
	const entries = 2048
	baseline := proto.Manifest{Entries: make([]proto.ManifestEntry, 0, entries)}
	current := proto.Manifest{Entries: make([]proto.ManifestEntry, 0, entries)}
	for i := range entries {
		name := fmt.Sprintf("artifact-%05d", i)
		baseline.Entries = append(baseline.Entries, proto.ManifestEntry{
			Path: name, Type: proto.EntryFile, Mode: 0o600, SHA256: strings.Repeat("a", 64),
		})
		current.Entries = append(current.Entries, proto.ManifestEntry{
			Path: name, Type: proto.EntryFile, Mode: 0o600, SHA256: strings.Repeat("b", 64),
		})
	}
	bundle, err := workspaceDelta(context.Background(), baseline, current, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Paths) != entries || len(bundle.RemoteManifest.Entries) != entries {
		t.Fatalf("workspaceDelta() returned %d paths and %d entries, want %d each",
			len(bundle.Paths), len(bundle.RemoteManifest.Entries), entries)
	}
}

func TestCollectExcludesNestedGitMetadata(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "dist", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "dist", ".git", "config"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "artifact"), []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := CollectWorkspaceChangesContext(
		context.Background(), workspace, t.TempDir(), proto.Manifest{}, proto.SelectionPolicy{}, 1<<20,
	)
	if err != nil || !collected {
		t.Fatalf("CollectWorkspaceChangesContext() = %+v, %t, %v", bundle, collected, err)
	}
	if got, want := fmt.Sprint(bundle.Paths), "[artifact dist]"; got != want {
		t.Fatalf("retained paths = %s, want %s", got, want)
	}
	for _, entry := range bundle.RemoteManifest.Entries {
		if strings.Contains(entry.Path, ".git") {
			t.Fatalf("retained Git metadata entry %+v", entry)
		}
	}
}

func TestCleanupTempsRemovesInterruptedBaseCapture(t *testing.T) {
	jobDir := t.TempDir()
	for _, name := range []string{".changes-partial", ".change-base-partial"} {
		temp := filepath.Join(jobDir, name)
		if err := os.Mkdir(temp, 0o700); err != nil {
			t.Fatal(err)
		}
		restrictive := filepath.Join(temp, "restrictive")
		if err := os.Mkdir(restrictive, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(restrictive, "artifact"), []byte("retained"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(restrictive, 0o555); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(restrictive, 0o700)
	}
	if err := CleanupTemps(jobDir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".changes-partial", ".change-base-partial"} {
		if _, err := os.Lstat(filepath.Join(jobDir, name)); !os.IsNotExist(err) {
			t.Fatalf("temporary tree %q survived cleanup: %v", name, err)
		}
	}
}

func TestRecoverApplicationRollsBackRestrictiveInstall(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	baseline := proto.Manifest{}
	if err := CaptureWorkspaceBaseContext(context.Background(), remote, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	sealed := filepath.Join(remote, "sealed")
	if err := os.Mkdir(sealed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sealed, "artifact"), []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealed, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(sealed, 0o700)

	bundle, collected, err := CollectWorkspaceChangesContext(
		context.Background(), remote, jobDir, baseline, proto.SelectionPolicy{}, 1<<20,
	)
	if err != nil || !collected {
		t.Fatalf("CollectWorkspaceChangesContext() = %+v, %t, %v", bundle, collected, err)
	}
	staged := extractTestBundle(t, jobDir, bundle)
	defer os.Chmod(filepath.Join(staged, "remote", "sealed"), 0o700)
	local := t.TempDir()
	result, err := Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction())
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(local, "sealed"), 0o700)
	journal, err := loadApplyJournal(local, result.Transaction)
	if err != nil {
		t.Fatal(err)
	}
	journal.Phase = applyPhasePrepared
	if err := writeApplyJournal(local, journal); err != nil {
		t.Fatal(err)
	}
	pending, err := RecoverApplication(local, result.Transaction)
	if err != nil {
		t.Fatal(err)
	}
	if pending != nil {
		t.Fatalf("rolled-back transaction returned pending state: %+v", pending)
	}
	if _, err := os.Lstat(filepath.Join(local, "sealed")); !os.IsNotExist(err) {
		t.Fatalf("restrictive installed tree survived rollback: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(local, result.Transaction)); !os.IsNotExist(err) {
		t.Fatalf("restrictive transaction survived rollback: %v", err)
	}
}

func TestValidateBundleRejectsOverlappingChangeRoots(t *testing.T) {
	bundle := proto.ChangeBundle{
		V: BundleVersion, BaselineRoot: (proto.Manifest{}).RootHash(),
		Paths: []string{"build", "build/out"},
		RemoteManifest: proto.Manifest{Entries: []proto.ManifestEntry{
			{Path: "build", Type: proto.EntryDir, Mode: 0o755},
			{Path: "build/out", Type: proto.EntryDir, Mode: 0o755},
		}},
	}
	if err := ValidateBundle(bundle); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("ValidateBundle() overlapping roots error = %v", err)
	}
}

func TestValidateBundleClassifiesEntryLimit(t *testing.T) {
	bundle := proto.ChangeBundle{
		V: BundleVersion, BaselineRoot: (proto.Manifest{}).RootHash(),
		BaseManifest: proto.Manifest{Entries: make([]proto.ManifestEntry, MaxChangeEntries+1)},
	}
	if err := ValidateBundle(bundle); !errors.Is(err, ErrEntryLimitExceeded) {
		t.Fatalf("ValidateBundle() error = %v, want ErrEntryLimitExceeded", err)
	}
}

func TestValidateBundleRejectsCaseFoldedOverlappingChangeRoots(t *testing.T) {
	bundle := proto.ChangeBundle{
		V: BundleVersion, BaselineRoot: (proto.Manifest{}).RootHash(),
		Paths: []string{"Build", "build/out"},
		RemoteManifest: proto.Manifest{Entries: []proto.ManifestEntry{
			{Path: "Build", Type: proto.EntryDir, Mode: 0o755},
		}},
	}
	if err := ValidateBundle(bundle); err == nil || !strings.Contains(err.Error(), "case-insensitive") {
		t.Fatalf("ValidateBundle() case-folded overlap error = %v", err)
	}
}

func TestValidateBundleRejectsUnchangedRoot(t *testing.T) {
	manifest := proto.Manifest{Entries: []proto.ManifestEntry{
		{Path: "artifact", Type: proto.EntryFile, Mode: 0o600, SHA256: strings.Repeat("a", 64)},
	}}
	bundle := proto.ChangeBundle{
		V: BundleVersion, BaselineRoot: (proto.Manifest{}).RootHash(),
		Paths: []string{"artifact"}, BaseManifest: manifest, RemoteManifest: manifest,
	}
	if err := ValidateBundle(bundle); err == nil || !strings.Contains(err.Error(), "unchanged") {
		t.Fatalf("ValidateBundle() unchanged path error = %v", err)
	}
}

func TestValidateBundleRejectsNestedGitMetadata(t *testing.T) {
	bundle := proto.ChangeBundle{
		V: BundleVersion, BaselineRoot: (proto.Manifest{}).RootHash(), Paths: []string{"dist"},
		RemoteManifest: proto.Manifest{Entries: []proto.ManifestEntry{
			{Path: "dist", Type: proto.EntryDir, Mode: 0o755},
			{Path: "dist/.git", Type: proto.EntryDir, Mode: 0o755},
		}},
	}
	if err := ValidateBundle(bundle); err == nil || !strings.Contains(err.Error(), "Git metadata") {
		t.Fatalf("ValidateBundle() nested Git metadata error = %v", err)
	}
}

func TestValidateBundleRejectsCaseCollidingManifestPaths(t *testing.T) {
	bundle := proto.ChangeBundle{
		V: BundleVersion, BaselineRoot: (proto.Manifest{}).RootHash(), Paths: []string{"dist"},
		RemoteManifest: proto.Manifest{Entries: []proto.ManifestEntry{
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

func TestRecoverApplicationRollsBackInterruptedInstall(t *testing.T) {
	local, bundle, staged := applyFixture(t, "old", "new")
	transaction := NewApplyTransaction()
	result, err := Apply(staged, local, bundle, nil, "test-owner", transaction)
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

func TestApplyRetainsRecoveryJournalUntilCommit(t *testing.T) {
	remote := t.TempDir()
	artifact := filepath.Join(remote, "artifact")
	f, err := os.OpenFile(artifact, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 20); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	jobDir := t.TempDir()
	bundle, _, err := collectTestChanges(remote, jobDir, []testChangeRoot{{
		Path: "artifact",
	}}, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	staged := extractTestBundle(t, jobDir, bundle)
	local := t.TempDir()
	transaction := NewApplyTransaction()
	result, err := Apply(staged, local, bundle, nil, "test-owner", transaction)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := loadApplyJournal(local, transaction)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Phase != applyPhaseCommitted || len(journal.Items) != 1 {
		t.Fatalf("retained journal = %+v", journal)
	}
	if err := CommitApply(local, result.Transaction); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(local, transaction)); !os.IsNotExist(err) {
		t.Fatalf("committed transaction survived: %v", err)
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
		t.Fatalf("original change changed = %q, %v", got, err)
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
	parentID := mustChangeParentIdentity(t, root, "artifact")
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
	local, bundle, staged := applyFixture(t, "old", "new")
	transaction := NewApplyTransaction()
	if _, err := Apply(staged, local, bundle, nil, "test-owner", transaction); err != nil {
		t.Fatal(err)
	}
	pending, err := RecoverApplication(local, transaction)
	if err != nil {
		t.Fatal(err)
	}
	if pending == nil || pending.Owner != "test-owner" || pending.BundleRoot != bundle.RootHash() || len(pending.Paths) != 1 {
		t.Fatalf("pending committed transaction = %+v", pending)
	}
	if err := CommitApply(local, transaction); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverApplicationContextRefusesCanceledRecovery(t *testing.T) {
	local, bundle, staged := applyFixture(t, "old", "new")
	transaction := NewApplyTransaction()
	if _, err := Apply(staged, local, bundle, nil, "test-owner", transaction); err != nil {
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

func TestRecoverApplicationPreservesBackupWhenInstalledChangeChanged(t *testing.T) {
	local, bundle, staged := applyFixture(t, "old", "new")
	transaction := NewApplyTransaction()
	if _, err := Apply(staged, local, bundle, nil, "test-owner", transaction); err != nil {
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
	local, bundle, staged := applyFixture(t, "old", "new")
	transaction := NewApplyTransaction()
	if _, err := Apply(staged, local, bundle, nil, "test-owner", transaction); err != nil {
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

func TestInstalledChangeValidationRejectsConcurrentEdit(t *testing.T) {
	local, bundle, staged := applyFixture(t, "old", "new")
	transaction := NewApplyTransaction()
	if _, err := Apply(staged, local, bundle, nil, "test-owner", transaction); err != nil {
		t.Fatal(err)
	}
	journal, err := loadApplyJournal(local, transaction)
	if err != nil {
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
	if err := validateInstalledChanges(root, journal); err == nil || !strings.Contains(err.Error(), "changed after installation") {
		t.Fatalf("installed validation error = %v", err)
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
	item.Parent = mustChangeParentIdentity(t, local, item.Path)
	journal.Items = []applyJournalItem{item}
	parentDir, err := openChangeParent(root, item.Path, item.Parent)
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

func TestOpenChangeParentRejectsReplacementAfterValidation(t *testing.T) {
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
	want := mustChangeParentIdentity(t, rootPath, "nested/artifact")
	if err := os.Rename(parent, filepath.Join(rootPath, "original-parent")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}

	dir, err := openChangeParent(root, "nested/artifact", want)
	if dir != nil {
		dir.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "replaced parent directory") {
		t.Fatalf("openChangeParent() error = %v", err)
	}
}

func TestPinnedChangeParentCannotRedirectInstallation(t *testing.T) {
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
	want := mustChangeParentIdentity(t, rootPath, "nested/artifact")
	destinationDir, err := openChangeParent(root, "nested/artifact", want)
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
	if err := verifyChangeParent(root, item.Path, want); err == nil {
		t.Fatal("parent replacement was not detected after installation")
	}
}

func TestPinnedChangeParentCannotRedirectRollback(t *testing.T) {
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
	want := mustChangeParentIdentity(t, rootPath, "nested/artifact")
	destinationDir, err := openChangeParent(root, "nested/artifact", want)
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
	parentID, err := captureChangeParentIdentity(destination.root, "artifact")
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
	workspace, bundle, staged := applyFixture(t, "old", "new")
	parent := t.TempDir()
	originalWorkspace := workspace
	workspace = filepath.Join(parent, "workspace")
	if err := os.Rename(originalWorkspace, workspace); err != nil {
		t.Fatal(err)
	}
	workspaceID, _, err := fsidentity.Lstat(workspace)
	if err != nil {
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
		staged, workspace, bundle, nil, "test-owner", transaction, workspaceID,
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
	workspace, bundle, staged := applyFixture(t, "old", "new")
	parent := t.TempDir()
	originalWorkspace := workspace
	workspace = filepath.Join(parent, "workspace")
	if err := os.Rename(originalWorkspace, workspace); err != nil {
		t.Fatal(err)
	}
	workspaceID, _, err := fsidentity.Lstat(workspace)
	if err != nil {
		t.Fatal(err)
	}
	transaction := NewApplyTransaction()
	if _, err := ApplyToWorkspace(
		staged, workspace, bundle, nil, "test-owner", transaction, workspaceID,
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

	if _, err := ChangePathMatchesWorkspace(workspace, workspaceID, bundle, "artifact"); err == nil || !strings.Contains(err.Error(), "not the workspace") {
		t.Fatalf("ChangePathMatchesWorkspace() error = %v", err)
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

func TestRecoverApplicationRemovesParentsCreatedForMissingChange(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	changePath := "nested/deep/artifact"
	if err := os.MkdirAll(filepath.Join(remote, "nested", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, filepath.FromSlash(changePath)), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := collectTestChanges(remote, jobDir, []testChangeRoot{{Path: changePath}}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	staged := extractTestBundle(t, jobDir, bundle)
	transaction := NewApplyTransaction()
	if _, err := Apply(staged, local, bundle, nil, "test-owner", transaction); err != nil {
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
		t.Fatalf("created change parents survived rollback: %v", err)
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

func TestEnsureChangeParentsReportsOnlyDirectoriesItCreates(t *testing.T) {
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

	policies := map[string]applyParentPolicy{"nested/deep": {mode: 0o700}}
	if err := ensureChangeParents(root, &journal, "nested/deep/artifact", policies); err != nil {
		t.Fatal(err)
	}
	if len(journal.CreatedParents) != 1 || journal.CreatedParents[0].Path != "nested/deep" || journal.CreatedParents[0].Identity.IsZero() {
		t.Fatalf("ensureChangeParents() created = %v, want [nested/deep]", journal.CreatedParents)
	}
	if err := ensureChangeParents(root, &journal, "nested/deep/other", policies); err != nil {
		t.Fatal(err)
	}
	if len(journal.CreatedParents) != 1 {
		t.Fatalf("second ensureChangeParents() created = %v, want unchanged", journal.CreatedParents)
	}
}

func applyFixture(t *testing.T, localValue, remoteValue string) (string, proto.ChangeBundle, string) {
	t.Helper()
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte(localValue), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, err := snapshot.Build(remote, []string{"artifact"})
	if err != nil {
		t.Fatal(err)
	}
	if err := CaptureWorkspaceBaseContext(context.Background(), remote, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte(remoteValue), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := CollectWorkspaceChangesContext(context.Background(), remote, jobDir, baseline, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "artifact"), []byte(localValue), 0o600); err != nil {
		t.Fatal(err)
	}
	return local, bundle, extractTestBundle(t, jobDir, bundle)
}

func mustChangeParentIdentity(t *testing.T, rootPath, changePath string) fsidentity.Identity {
	t.Helper()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	identity, err := captureChangeParentIdentity(root, changePath)
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
	baselines, err := captureTestBaselines(root, []testChangeRoot{{Path: "dist/app"}, {Path: "report.json"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(baselines) != 2 || baselines[0].Digest == "" || baselines[0].Missing || !baselines[1].Missing {
		t.Fatalf("captureTestBaselines() = %+v", baselines)
	}
	if err := os.WriteFile(filepath.Join(root, "dist", "app"), []byte("changed"), 0o755); err != nil {
		t.Fatal(err)
	}
	current, err := captureTestBaselines(root, []testChangeRoot{{Path: "dist/app"}})
	if err != nil {
		t.Fatal(err)
	}
	if current[0].Digest == baselines[0].Digest {
		t.Fatal("content change did not change the baseline")
	}
}

func TestCollectCommitsSelectedChangesAsImmutableBundle(t *testing.T) {
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
	bundle, collected, err := collectTestChanges(workspace, jobDir, []testChangeRoot{
		{Path: "dist"},
		{Path: "failure.log"},
	}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !collected || bundle.V != BundleVersion || bundle.Bytes != 11 || len(bundle.Paths) != 2 {
		t.Fatalf("collectTestChanges() = collected %t, bundle %+v", collected, bundle)
	}
	loaded, err := Load(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RootHash() != bundle.RootHash() {
		t.Fatalf("loaded bundle root = %s, want %s", loaded.RootHash(), bundle.RootHash())
	}
	for name, openArchive := range map[string]func(string) (*os.File, error){
		"base": OpenBaseArchive, "remote": OpenRemoteArchive,
	} {
		archiveFile, err := openArchive(jobDir)
		if err != nil {
			t.Fatal(err)
		}
		if info, err := archiveFile.Stat(); err != nil || info.Size() == 0 {
			archiveFile.Close()
			t.Fatalf("%s archive stat = %v, %v", name, info, err)
		}
		if err := archiveFile.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCollectHonorsSizeLimit(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "success.bin"), []byte("success"), 0o600); err != nil {
		t.Fatal(err)
	}
	jobDir := t.TempDir()
	if _, _, err := collectTestChanges(workspace, jobDir, []testChangeRoot{
		{Path: "success.bin"},
	}, 3); err == nil {
		t.Fatal("oversized change collection succeeded")
	}
	if _, err := os.Stat(filepath.Join(jobDir, BundleDirectory)); !os.IsNotExist(err) {
		t.Fatalf("failed collection committed change directory: %v", err)
	}
}

func TestExtractAndApplyRefusesChangedDestination(t *testing.T) {
	local, bundle, staged := applyFixture(t, "old\n", "remote edit\n")
	if err := os.WriteFile(filepath.Join(local, "artifact"), []byte("user edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Apply(staged, local, bundle, map[string]bool{"artifact": true}, "test-owner", NewApplyTransaction())
	var conflict *MergeConflictError
	if !errors.As(err, &conflict) || fmt.Sprint(conflict.Paths) != "[artifact]" {
		t.Fatalf("Apply conflict = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(local, "artifact"))
	if err != nil || !bytes.Equal(got, []byte("user edit")) {
		t.Fatalf("destination after conflict = %q, %v", got, err)
	}
	entries, err := os.ReadDir(local)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), applyTransactionPrefix) {
			t.Fatalf("conflicting apply published transaction %q", entry.Name())
		}
	}
}

func TestApplyThreeWayMergesIndependentTextEdits(t *testing.T) {
	base := "first\nsecond\nthird\n"
	local, bundle, staged := applyFixture(t, base, "FIRST\nsecond\nthird\n")
	if err := os.WriteFile(filepath.Join(local, "artifact"), []byte("first\nsecond\nTHIRD\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction())
	if err != nil {
		t.Fatal(err)
	}
	if err := CommitApply(local, result.Transaction); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(local, "artifact"))
	if err != nil || string(got) != "FIRST\nsecond\nTHIRD\n" {
		t.Fatalf("merged artifact = %q, %v", got, err)
	}
}

func TestExtractAndApplyReplacesMatchingDestination(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(remote, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "dist", "app"), []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	baseline, err := snapshot.Build(remote, []string{"dist", "dist/app"})
	if err != nil {
		t.Fatal(err)
	}
	if err := CaptureWorkspaceBaseContext(context.Background(), remote, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "dist", "app"), []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := CollectWorkspaceChangesContext(context.Background(), remote, jobDir, baseline, proto.SelectionPolicy{}, 1<<20)
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
	staged := extractTestBundle(t, jobDir, bundle)
	result, err := Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 1 || result.Applied[0] != "dist/app" {
		t.Fatalf("Apply result = %+v", result)
	}
	if err := CommitApply(local, result.Transaction); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(local, "dist", "app"))
	if err != nil || !bytes.Equal(got, []byte("new")) {
		t.Fatalf("applied change = %q, %v", got, err)
	}
	info, err := os.Stat(filepath.Join(local, "dist", "app"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("applied change mode = %v, %v", info, err)
	}
}

func TestApplyRefusesLocallyModifiedStaging(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("authentic"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := collectTestChanges(remote, jobDir, []testChangeRoot{{Path: "artifact"}}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	staged := extractTestBundle(t, jobDir, bundle)
	if err := os.WriteFile(filepath.Join(staged, "remote", "artifact"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction()); err == nil || !strings.Contains(err.Error(), "changed during pack") {
		t.Fatalf("Apply tampered staging error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(local, "artifact")); !os.IsNotExist(err) {
		t.Fatalf("tampered change reached destination: %v", err)
	}
}

func TestVerifiedMergeInputsAreIndependentOfStaging(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("authentic"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := collectTestChanges(remote, jobDir, []testChangeRoot{{Path: "artifact"}}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	staged := extractTestBundle(t, jobDir, bundle)
	trusted := t.TempDir()
	accesses, err := materializeVerifiedMergeInputs(staged, trusted, bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTreeAccesses(accesses)
	if err := os.WriteFile(filepath.Join(staged, "remote", "artifact"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(trusted, "remote", "artifact"))
	if err != nil || string(got) != "authentic" {
		t.Fatalf("trusted remote input = %q, %v", got, err)
	}
}

func TestApplyTreatsDivergentBinaryFileAsConflict(t *testing.T) {
	base := "base\x00middle\nsuffix\n"
	local, bundle, staged := applyFixture(t, base, "remote\x00middle\nsuffix\n")
	localValue := []byte("base\x00middle\nlocal\n")
	if err := os.WriteFile(filepath.Join(local, "artifact"), localValue, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction())
	var conflict *MergeConflictError
	if !errors.As(err, &conflict) || fmt.Sprint(conflict.Paths) != "[artifact]" {
		t.Fatalf("binary apply error = %v, want artifact conflict", err)
	}
	got, readErr := os.ReadFile(filepath.Join(local, "artifact"))
	if readErr != nil || !bytes.Equal(got, localValue) {
		t.Fatalf("binary local value changed = %q, %v", got, readErr)
	}
}

func TestMergeRegularFileReportsGitOperationalFailure(t *testing.T) {
	bin := t.TempDir()
	git := filepath.Join(bin, "git")
	if err := os.WriteFile(git, []byte("#!/bin/sh\necho helper-exploded >&2\nexit 255\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	root := t.TempDir()
	for name, value := range map[string]string{"ours": "ours\n", "base": "base\n", "remote": "remote\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	clean, err := mergeRegularFile(
		context.Background(), filepath.Join(root, "ours"), filepath.Join(root, "base"),
		filepath.Join(root, "remote"), filepath.Join(root, "merged"), 0o600,
	)
	if clean || err == nil || !strings.Contains(err.Error(), "helper-exploded") {
		t.Fatalf("mergeRegularFile() = clean %t, error %v", clean, err)
	}
}

func TestCommitApplyRefusesReplacementTransactionDirectory(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := collectTestChanges(remote, jobDir, []testChangeRoot{{
		Path: "artifact",
	}}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	staged := extractTestBundle(t, jobDir, bundle)
	result, err := Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction())
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

func TestCommitApplyPreservesBackupAfterConcurrentEdit(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := collectTestChanges(remote, jobDir, []testChangeRoot{{Path: "artifact"}}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	staged := extractTestBundle(t, jobDir, bundle)
	result, err := Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "artifact"), []byte("edited-after-apply"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CommitApply(local, result.Transaction); err == nil {
		t.Fatal("CommitApply discarded recovery data after a concurrent edit")
	}
	if _, err := os.Lstat(filepath.Join(local, result.Transaction)); err != nil {
		t.Fatalf("recovery transaction was removed: %v", err)
	}
}

func TestApplyCreatesMissingParentsInsideDestination(t *testing.T) {
	remote := t.TempDir()
	jobDir := t.TempDir()
	changePath := "nested/deep/artifact"
	if err := os.MkdirAll(filepath.Join(remote, "nested", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(remote, "nested"), 0o710); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(remote, "nested", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, filepath.FromSlash(changePath)), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := collectTestChanges(remote, jobDir, []testChangeRoot{{Path: changePath}}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	staged := extractTestBundle(t, jobDir, bundle)
	result, err := Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction())
	if err != nil {
		t.Fatal(err)
	}
	if err := CommitApply(local, result.Transaction); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(local, filepath.FromSlash(changePath)))
	if err != nil || string(got) != "new" {
		t.Fatalf("nested applied change = %q, %v", got, err)
	}
	for name, want := range map[string]os.FileMode{"nested": 0o710, "nested/deep": 0o700} {
		info, statErr := os.Stat(filepath.Join(local, filepath.FromSlash(name)))
		if statErr != nil || info.Mode().Perm() != want {
			t.Fatalf("created parent %s mode = %v, %v; want %o", name, info, statErr, want)
		}
	}
}
