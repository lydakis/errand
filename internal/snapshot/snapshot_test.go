package snapshot

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestManifestDeterministic(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "b.txt", "bee")
	writeFile(t, root, "a/one.txt", "one")

	paths, _, _, err := SelectFilesWithOptions(root, SelectOptions{IncludeAll: true})
	if err != nil {
		t.Fatal(err)
	}
	m1, err := Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	if m1.RootHash() != m2.RootHash() {
		t.Fatal("same tree produced different root hashes")
	}
	writeFile(t, root, "b.txt", "changed")
	m3, err := Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	if m3.RootHash() == m1.RootHash() {
		t.Fatal("changed content did not change the root hash")
	}
}

func TestBuildBoundedRefusesOversizedFileBeforeHashing(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "large.bin", "too large")
	_, err := BuildBounded(root, []string{"large.bin"}, 3, 10)
	if !errors.Is(err, ErrByteLimitExceeded) {
		t.Fatalf("BuildBounded() error = %v, want byte limit exceeded", err)
	}
}

func TestBuildBoundedClassifiesEntryLimit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "one", "1")
	writeFile(t, root, "two", "2")
	_, err := BuildBounded(root, []string{"one", "two"}, 1<<20, 1)
	if !errors.Is(err, ErrEntryLimitExceeded) {
		t.Fatalf("BuildBounded() error = %v, want entry limit exceeded", err)
	}
}

func TestSelectFilesExcludesLocalChangeTransactions(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	writeFile(t, root, "kept.txt", "kept")
	writeFile(t, root, ".errand-change-not-a-transaction/kept.txt", "ordinary user data")
	writeFile(t, root, ".errand-change-01ARZ3NDEKTSV4RRFFQ69G5FAV/journal.json", "sensitive rollback state")
	paths, _, _, err := SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(paths, ".errand-change-01ARZ3NDEKTSV4RRFFQ69G5FAV/journal.json") {
		t.Fatalf("snapshot included change transaction: %v", paths)
	}
	if !slices.Contains(paths, "kept.txt") {
		t.Fatalf("snapshot omitted ordinary file: %v", paths)
	}
	if !slices.Contains(paths, ".errand-change-not-a-transaction/kept.txt") {
		t.Fatalf("snapshot over-excluded an ordinary prefixed path: %v", paths)
	}
}

func TestBuildRecordsImplicitParentDirectoryModes(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	writeFile(t, root, "readonly/file.txt", "content")
	readonly := filepath.Join(root, "readonly")
	if err := os.Chmod(readonly, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readonly, 0o700) })
	paths, _, _, err := SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range m.Entries {
		if e.Path == "readonly" {
			if e.Type != "dir" || e.Mode != 0o555 {
				t.Fatalf("parent directory manifest entry = %+v", e)
			}
			return
		}
	}
	t.Fatal("implicit parent directory was omitted from the manifest")
}

func TestPackDetectsChangeDuringSnapshot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "original")
	paths, _, _, err := SelectFilesWithOptions(root, SelectOptions{IncludeAll: true})
	if err != nil {
		t.Fatal(err)
	}
	m, err := Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	// The file changes between manifest and pack: consistent-or-refused.
	writeFile(t, root, "a.txt", "MUTATED!")
	var buf bytes.Buffer
	err = Pack(&buf, root, m)
	if err == nil || !strings.Contains(err.Error(), "changed during pack") {
		t.Fatalf("expected changed-during-pack refusal, got %v", err)
	}
}

func TestPackPartialRevalidatesCachedFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "cached.txt", "before")
	manifest, err := Build(root, []string{"cached.txt"})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "cached.txt", "after!")

	var packed bytes.Buffer
	err = PackPartial(&packed, root, manifest, func(proto.ManifestEntry) bool { return false })
	if err == nil || !strings.Contains(err.Error(), "changed during pack") {
		t.Fatalf("cached file mutation error = %v", err)
	}
}

type blockingPackWriter struct {
	buf     bytes.Buffer
	writes  int
	blocked chan struct{}
	release chan struct{}
}

func (w *blockingPackWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == 2 {
		close(w.blocked)
		<-w.release
	}
	return w.buf.Write(p)
}

func TestPackDetectsAppendDuringFileCopy(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", strings.Repeat("a", 64<<10))
	m, err := Build(root, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	w := &blockingPackWriter{blocked: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- Pack(w, root, m) }()
	select {
	case <-w.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("pack did not reach the file payload")
	}
	f, err := os.OpenFile(filepath.Join(root, "a.txt"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("appended"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	close(w.release)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "changed during pack") {
			t.Fatalf("append-during-pack error = %v, want changed-during-pack refusal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pack did not finish after release")
	}
}

func TestPackDetectsRetargetedSymlink(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "one", "1")
	writeFile(t, root, "two", "2")
	if err := os.Symlink("one", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	m, err := Build(root, []string{"link", "one", "two"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("two", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Pack(&buf, root, m); err == nil || !strings.Contains(err.Error(), "changed during pack") {
		t.Fatalf("retargeted symlink pack error = %v", err)
	}
}

func TestPackRefusesDirectoryReplacedByEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, root, "dir/file", "inside")
	writeFile(t, outside, "file", "outside-secret")
	m, err := Build(root, []string{"dir", "dir/file"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err = Pack(&buf, root, m)
	if err == nil || !strings.Contains(err.Error(), "changed during pack") {
		t.Fatalf("escaping ancestor pack error = %v", err)
	}
	if bytes.Contains(buf.Bytes(), []byte("outside-secret")) {
		t.Fatal("pack streamed bytes from outside the snapshot root")
	}
}

func TestErrandignoreSelection(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".errandignore", "*.log\nbuild/\n")
	writeFile(t, root, "keep.txt", "k")
	writeFile(t, root, "noise.log", "n")
	writeFile(t, root, "build/out.bin", "b")
	writeFile(t, root, ".git/config", "never ship")
	writeFile(t, root, ".GIT/config", "also never ship")
	writeFile(t, root, "nested/.git", "gitdir: /outside/worktree")

	paths, _, policy, err := SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(paths, ",")
	if !strings.Contains(joined, "keep.txt") {
		t.Fatalf("keep.txt missing from %v", paths)
	}
	for _, banned := range []string{"noise.log", "build/out.bin", ".git/config", ".GIT/config", "nested/.git"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("%s should have been excluded: %v", banned, paths)
		}
	}
	matcher, err := pathpolicy.Compile(policy)
	if err != nil {
		t.Fatal(err)
	}
	if !matcher.Ignored("future.log", false) || !matcher.Ignored("build/future.bin", false) {
		t.Fatalf("frozen policy does not preserve .errandignore: %+v", policy)
	}
}

func TestGitSelectionPolicyIncludesAncestorRulesForSubdirectoryRoot(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	worktree := t.TempDir()
	if out, err := exec.Command("git", "-C", worktree, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	writeFile(t, worktree, ".gitignore", "/pkg/*.secret\n")
	writeFile(t, worktree, "pkg/keep.txt", "kept")
	writeFile(t, worktree, "pkg/already.secret", "ignored")

	root := filepath.Join(worktree, "pkg")
	paths, _, policy, err := SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths, []string{"keep.txt"}) {
		t.Fatalf("subdirectory selection = %v, want [keep.txt]", paths)
	}
	matcher, err := pathpolicy.Compile(policy)
	if err != nil {
		t.Fatal(err)
	}
	if !matcher.Ignored("created.secret", false) {
		t.Fatalf("frozen policy lost ancestor .gitignore: %+v", policy)
	}
}

func TestGitSelectionPolicyFollowsConfiguredExcludesSymlink(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	excludes := filepath.Join(t.TempDir(), "global-ignore")
	writeFile(t, filepath.Dir(excludes), filepath.Base(excludes), "*.secret\n")
	configured := filepath.Join(t.TempDir(), "configured-ignore")
	if err := os.Symlink(excludes, configured); err != nil {
		t.Skipf("creating excludes symlink: %v", err)
	}
	if out, err := exec.Command("git", "-C", root, "config", "core.excludesFile", configured).CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	writeFile(t, root, "keep.txt", "kept")
	writeFile(t, root, "existing.secret", "ignored")

	paths, _, policy, err := SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths, []string{"keep.txt"}) {
		t.Fatalf("selection = %v, want [keep.txt]", paths)
	}
	matcher, err := pathpolicy.Compile(policy)
	if err != nil {
		t.Fatal(err)
	}
	if !matcher.Ignored("created.secret", false) {
		t.Fatalf("frozen policy lost symlinked core.excludesFile: %+v", policy)
	}
}

func TestGitSelectionPolicyResolvesRelativeConfiguredExcludesFromWorktree(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	worktree := t.TempDir()
	if out, err := exec.Command("git", "-C", worktree, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	writeFile(t, worktree, "relative-ignore", "*.secret\n")
	if out, err := exec.Command("git", "-C", worktree, "config", "core.excludesFile", "relative-ignore").CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	writeFile(t, worktree, "pkg/keep.txt", "kept")
	writeFile(t, worktree, "pkg/existing.secret", "ignored")

	paths, _, policy, err := SelectFiles(filepath.Join(worktree, "pkg"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths, []string{"keep.txt"}) {
		t.Fatalf("selection = %v, want [keep.txt]", paths)
	}
	matcher, err := pathpolicy.Compile(policy)
	if err != nil {
		t.Fatal(err)
	}
	if !matcher.Ignored("created.secret", false) {
		t.Fatalf("frozen policy lost relative core.excludesFile: %+v", policy)
	}
}

func TestGitSelectionPolicyFollowsInfoExcludeSymlink(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	excludes := filepath.Join(t.TempDir(), "info-exclude")
	writeFile(t, filepath.Dir(excludes), filepath.Base(excludes), "*.secret\n")
	infoExclude := filepath.Join(root, ".git", "info", "exclude")
	if err := os.Remove(infoExclude); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(excludes, infoExclude); err != nil {
		t.Skipf("creating info/exclude symlink: %v", err)
	}
	writeFile(t, root, "keep.txt", "kept")
	writeFile(t, root, "existing.secret", "ignored")

	paths, _, policy, err := SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths, []string{"keep.txt"}) {
		t.Fatalf("selection = %v, want [keep.txt]", paths)
	}
	matcher, err := pathpolicy.Compile(policy)
	if err != nil {
		t.Fatal(err)
	}
	if !matcher.Ignored("created.secret", false) {
		t.Fatalf("frozen policy lost symlinked info/exclude: %+v", policy)
	}
}

func TestNonGitSnapshotRequiresExplicitPolicyOrOverride(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "secret.env", "credential")

	if _, _, _, err := SelectFiles(root); err == nil || !strings.Contains(err.Error(), "--include-all") {
		t.Fatalf("implicit non-Git snapshot error = %v, want explicit override guidance", err)
	}
	paths, gi, _, err := SelectFilesWithOptions(root, SelectOptions{IncludeAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if gi.Repository || !slices.Contains(paths, "secret.env") {
		t.Fatalf("explicit non-Git snapshot = paths %v, git %+v", paths, gi)
	}
}

func TestPackEmptyManifestDoesNotInspectRoot(t *testing.T) {
	var packed bytes.Buffer
	missingRoot := filepath.Join(t.TempDir(), "does-not-exist")
	if err := Pack(&packed, missingRoot, proto.Manifest{}); err != nil {
		t.Fatalf("packing empty manifest: %v", err)
	}
	if _, err := tar.NewReader(&packed).Next(); err != io.EOF {
		t.Fatalf("empty archive first entry error = %v, want EOF", err)
	}
}

func TestSnapshotAlwaysRefusesFilesystemRoot(t *testing.T) {
	root := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	if _, _, _, err := SelectFilesWithOptions(root, SelectOptions{IncludeAll: true}); err == nil ||
		!strings.Contains(err.Error(), "filesystem root") {
		t.Fatalf("filesystem-root snapshot error = %v", err)
	}
}

func TestHomeSnapshotRequiresOverrideEvenWithPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, home, ".errandignore", "*.tmp\n")
	writeFile(t, home, "keep.txt", "safe")

	if _, _, _, err := SelectFiles(home); err == nil || !strings.Contains(err.Error(), "home directory") {
		t.Fatalf("home snapshot error = %v, want explicit override requirement", err)
	}
	paths, _, _, err := SelectFilesWithOptions(home, SelectOptions{IncludeAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(paths, "keep.txt") {
		t.Fatalf("explicit home snapshot omitted keep.txt: %v", paths)
	}
}

func TestErrandignoreReadFailureIsFatal(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".errandignore"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "secret.env", "must not be selected")
	if _, _, _, err := SelectFiles(root); err == nil || !strings.Contains(err.Error(), ".errandignore") {
		t.Fatalf("unreadable .errandignore error = %v", err)
	}
}

func TestBrokenErrandignoreSymlinkIsFatal(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("missing-policy", filepath.Join(root, ".errandignore")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "secret.env", "must not be selected")
	if _, _, _, err := SelectFiles(root); err == nil || !strings.Contains(err.Error(), ".errandignore") {
		t.Fatalf("broken .errandignore symlink error = %v", err)
	}
}

func TestGitSelectionFailsClosed(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "secret.env", "do not upload")
	want := errors.New("git index unavailable")

	_, _, _, err := selectFiles(root, GitInfo{Repository: true, Commit: "deadbeef"}, func(string) ([]string, error) {
		return nil, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("git selection error = %v, want %v", err, want)
	}
}

func TestUnbornGitRepositoryHonorsGitignore(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	writeFile(t, root, ".gitignore", "secret.env\n")
	writeFile(t, root, "secret.env", "credential")
	writeFile(t, root, "keep.txt", "safe")

	paths, gi, policy, err := SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if !gi.Repository || gi.Commit != "" {
		t.Fatalf("unborn repository info = %+v", gi)
	}
	joined := strings.Join(paths, ",")
	if strings.Contains(joined, "secret.env") || !strings.Contains(joined, "keep.txt") {
		t.Fatalf("unborn repository selection = %v", paths)
	}
	matcher, err := pathpolicy.Compile(policy)
	if err != nil {
		t.Fatal(err)
	}
	if !matcher.Ignored("secret.env", false) {
		t.Fatalf("Git selection did not freeze ignore policy: %+v", policy)
	}
}

func TestGitSelectionRebasesNestedGitignore(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	writeFile(t, root, "nested/.gitignore", "*.tmp\n/cache/\n")
	writeFile(t, root, "nested/keep.txt", "safe")
	paths, _, policy, err := SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(paths, "nested/keep.txt") {
		t.Fatalf("nested file missing from %v", paths)
	}
	matcher, err := pathpolicy.Compile(policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, ignored := range []string{"nested/direct.tmp", "nested/deeper/value.tmp", "nested/cache/value"} {
		if !matcher.Ignored(ignored, false) {
			t.Errorf("nested policy did not ignore %q: %+v", ignored, policy)
		}
	}
	if matcher.Ignored("other/value.tmp", false) {
		t.Fatalf("nested policy leaked outside its directory: %+v", policy)
	}
}

func TestGitSelectionFreezesIgnoredNestedGitignore(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	writeFile(t, root, ".gitignore", "nested/.gitignore\n")
	writeFile(t, root, "nested/.gitignore", "secret\n")

	paths, _, policy, err := SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(paths, "nested/.gitignore") {
		t.Fatalf("ignored nested policy was selected: %v", paths)
	}
	matcher, err := pathpolicy.Compile(policy)
	if err != nil {
		t.Fatal(err)
	}
	if !matcher.Ignored("nested/secret", false) {
		t.Fatalf("ignored nested policy was not frozen: %+v", policy)
	}
}

func TestSelectionGuardRejectsChangedIgnoredGitPolicy(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	writeFile(t, root, ".gitignore", "nested/.gitignore\n")
	writeFile(t, root, "nested/.gitignore", "secret\n")
	writeFile(t, root, "keep", "kept")
	_, _, _, guard, err := SelectFilesGuarded(root, SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "nested/.gitignore", "different-secret\n")
	if err := guard.Verify(); err == nil || !strings.Contains(err.Error(), "selection policy changed") {
		t.Fatalf("Verify() error = %v", err)
	}
}

func TestPolicyLinesDropsInertLines(t *testing.T) {
	got := policyLines([]byte("\n# comment\nvalue\r\n\\#literal\n"))
	if want := []string{"value", `\#literal`}; !slices.Equal(got, want) {
		t.Fatalf("policyLines() = %q, want %q", got, want)
	}
}

func TestRebasedNestedIgnorePatternStaysInsideItsDirectory(t *testing.T) {
	policy := proto.SelectionPolicy{Ignore: []string{
		rebaseIgnorePattern("nested", "foo/bar"),
		rebaseIgnorePattern("nested", "secret"),
	}}
	matcher, err := pathpolicy.Compile(policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path    string
		ignored bool
	}{
		{path: "nested/foo/bar", ignored: true},
		{path: "nested/deeper/secret", ignored: true},
		{path: "other/nested/foo/bar", ignored: false},
		{path: "other/nested/secret", ignored: false},
	} {
		if got := matcher.Ignored(test.path, false); got != test.ignored {
			t.Errorf("Ignored(%q) = %t, want %t (policy %v)", test.path, got, test.ignored, policy.Ignore)
		}
	}
}

func TestGitSubmoduleIsRejectedInsteadOfShippedEmpty(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	sub := filepath.Join(root, "deps", "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", sub, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("submodule git init: %v: %s", err, out)
	}
	writeFile(t, sub, "dep.txt", "dependency")
	if out, err := exec.Command("git", "-C", sub, "add", "dep.txt").CombinedOutput(); err != nil {
		t.Fatalf("submodule git add: %v: %s", err, out)
	}
	commit := exec.Command("git", "-C", sub, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "initial")
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("submodule git commit: %v: %s", err, out)
	}
	head, err := exec.Command("git", "-C", sub, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	cacheInfo := "160000," + strings.TrimSpace(string(head)) + ",deps/sub"
	if out, err := exec.Command("git", "-C", root, "update-index", "--add", "--cacheinfo", cacheInfo).CombinedOutput(); err != nil {
		t.Fatalf("record gitlink: %v: %s", err, out)
	}

	if _, _, _, err := SelectFiles(root); err == nil || !strings.Contains(err.Error(), "submodule") {
		t.Fatalf("initialized submodule selection error = %v", err)
	}
}
