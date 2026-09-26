package snapshot

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// prepareGitWatchFixture builds a Git-selected source with tracked, untracked,
// ignored-file and ignored-directory content. Selection changes below are made
// without event hints: the evidence alone must detect them.
func prepareGitWatchFixture(t *testing.T) (*Watch, *Builder, func(...string)) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	writeFile(t, root, ".gitignore", "secret\nbuild/\n*.log\n")
	writeFile(t, root, "value", "before")
	writeFile(t, root, "sub/tracked", "tracked")
	writeFile(t, root, "untracked", "untracked")
	writeFile(t, root, "secret", "private")
	writeFile(t, root, "build/output", "generated")
	writeFile(t, root, "logs/a.log", "log")
	git("init", "--quiet")
	git("add", ".gitignore", "value", "sub/tracked")
	git("commit", "--quiet", "-m", "fixture")
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	return &Watch{root: root, identity: info, Changed: make(chan struct{}, 1)}, new(Builder), git
}

// assertIncremental prepares after a content hint and fails if the watch fell
// back to full selection.
func assertIncremental(t *testing.T, w *Watch, b *Builder, name string) {
	t.Helper()
	before := w.prepared.fullAt
	w.invalidatePath(filepath.Join(w.root, name), dirtyContent)
	assertPreparedMatchesFull(t, w, b)
	if w.prepared.fullAt != before {
		t.Fatal("content-only edit used full Git selection")
	}
}

func TestGitWatchPrepareRefreshesContentEditsIncrementally(t *testing.T) {
	w, b, _ := prepareGitWatchFixture(t)
	assertPreparedMatchesFull(t, w, b)
	if w.prepared.evidence == nil || w.prepared.evidence.git == nil {
		t.Fatal("Git selection captured no evidence")
	}
	for i, name := range []string{"value", "untracked", "sub/tracked", "value"} {
		writeFile(t, w.root, name, strings.Repeat("x", i+1))
		assertIncremental(t, w, b, name)
	}
}

func TestGitWatchEvidenceDetectsSelectionChangesWithoutEvents(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, root string, git func(...string))
	}{
		{"gitignore-in-place", func(t *testing.T, root string, _ func(...string)) {
			writeFile(t, root, ".gitignore", "secret\nbuild/\n*.log\nvalue\n")
		}},
		{"nested-gitignore-reopens-file", func(t *testing.T, root string, _ func(...string)) {
			writeFile(t, root, "logs/.gitignore", "!a.log\n")
		}},
		{"info-exclude", func(t *testing.T, root string, _ func(...string)) {
			writeFile(t, root, ".git/info/exclude", "untracked\n")
		}},
		{"force-tracked-ignored-file", func(t *testing.T, _ string, git func(...string)) {
			git("add", "-f", "secret")
		}},
		{"untracked-file-created", func(t *testing.T, root string, _ func(...string)) {
			writeFile(t, root, "sub/new", "new")
		}},
		{"tracked-file-removed", func(t *testing.T, root string, _ func(...string)) {
			if err := os.Remove(filepath.Join(root, "sub", "tracked")); err != nil {
				t.Fatal(err)
			}
		}},
		{"excludes-file-configured", func(t *testing.T, root string, git func(...string)) {
			writeFile(t, root, "../excludes", "untracked\n")
			git("config", "core.excludesFile", filepath.Join(filepath.Dir(root), "excludes"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, b, git := prepareGitWatchFixture(t)
			guard := assertPreparedMatchesFull(t, w, b)
			tc.change(t, w.root, git)
			if err := guard.Verify(); err == nil {
				t.Fatal("guard accepted changed Git selection before event delivery")
			}
			// A content hint cannot authorize the stale selection.
			w.invalidatePath(filepath.Join(w.root, "value"), dirtyContent)
			assertPreparedMatchesFull(t, w, b)
		})
	}
}

func TestGitWatchEvidenceAcceptsIndexStatRefresh(t *testing.T) {
	w, b, _ := prepareGitWatchFixture(t)
	guard := assertPreparedMatchesFull(t, w, b)
	writeFile(t, w.root, "value", "edited")
	index := filepath.Join(w.root, ".git", "index")
	before, err := os.Lstat(index)
	if err != nil {
		t.Fatal(err)
	}
	// An editor's background status rewrites the index to refresh stat data.
	if out, err := exec.Command("git", "-C", w.root, "update-index", "--really-refresh").CombinedOutput(); err != nil && len(out) == 0 {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", w.root, "add", "value").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	after, err := os.Lstat(index)
	if err != nil {
		t.Fatal(err)
	}
	if sameFileEvidence(before, after) {
		t.Skip("index was not rewritten")
	}
	if err := guard.Verify(); err != nil {
		t.Fatalf("index rewrite with the same tracked set rejected selection: %v", err)
	}
	assertIncremental(t, w, b, "value")
}

func TestGitWatchEvidenceIgnoresExcludedDirectoryChurn(t *testing.T) {
	w, b, _ := prepareGitWatchFixture(t)
	guard := assertPreparedMatchesFull(t, w, b)
	if _, stamped := w.prepared.evidence.directories["build"]; stamped {
		t.Fatal("excluded build/ directory was stamped")
	}
	if _, stamped := w.prepared.evidence.directories["logs"]; !stamped {
		t.Fatal("logs/ can be reopened by a nested rule but was not stamped")
	}
	writeFile(t, w.root, "build/more", "generated")
	if err := guard.Verify(); err != nil {
		t.Fatalf("ignored build output invalidated selection: %v", err)
	}
	writeFile(t, w.root, "value", "edited")
	assertIncremental(t, w, b, "value")
}

func TestGitWatchEvidenceCoversIgnoreFilesAboveSubdirectoryRoot(t *testing.T) {
	parent, b, _ := prepareGitWatchFixture(t)
	root := filepath.Join(parent.root, "sub")
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "other", "other")
	w := &Watch{root: root, identity: info, Changed: make(chan struct{}, 1)}
	guard := assertPreparedMatchesFull(t, w, b)
	writeFile(t, parent.root, ".gitignore", "secret\nbuild/\n*.log\nother\n")
	if err := guard.Verify(); err == nil {
		t.Fatal("guard accepted an ancestor .gitignore change")
	}
	w.invalidatePath(filepath.Join(root, "other"), dirtyContent)
	assertPreparedMatchesFull(t, w, b)
}

// Git reports the repository config relative to the worktree, not to a
// subdirectory root.
func TestGitWatchEvidenceCoversRepositoryConfigFromSubdirectoryRoot(t *testing.T) {
	parent, b, git := prepareGitWatchFixture(t)
	root := filepath.Join(parent.root, "sub")
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "other", "other")
	writeFile(t, root, "dirty", "dirty")
	w := &Watch{root: root, identity: info, Changed: make(chan struct{}, 1)}
	guard := assertPreparedMatchesFull(t, w, b)
	dir := t.TempDir()
	writeFile(t, dir, "excludes", "other\n")
	git("config", "core.excludesFile", filepath.Join(dir, "excludes"))
	if err := guard.Verify(); err == nil {
		t.Fatal("guard accepted a repository config change")
	}
	w.invalidatePath(filepath.Join(root, "other"), dirtyContent)
	assertPreparedMatchesFull(t, w, b)
}

// absentConfigCase's declare makes Git consult a config file that does not
// exist yet and returns its path.
type absentConfigCase struct {
	name    string
	sub     bool // watch a subdirectory of the worktree
	declare func(t *testing.T, root string, git func(...string)) string
}

// assertCreatedConfigDetected checks that an unchanged absent config file keeps
// edits incremental, and that creating it to ignore an untracked file forces
// full selection. Another untracked file keeps the repository dirty, so its
// status does not change.
func assertCreatedConfigDetected(t *testing.T, cases []absentConfigCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, b, git := prepareGitWatchFixture(t)
			target := tc.declare(t, w.root, git)
			if tc.sub {
				writeFile(t, w.root, "sub/untracked", "untracked")
				root := filepath.Join(w.root, "sub")
				info, err := os.Lstat(root)
				if err != nil {
					t.Fatal(err)
				}
				w = &Watch{root: root, identity: info, Changed: make(chan struct{}, 1)}
			}
			writeFile(t, w.root, "dirty", "dirty")
			guard := assertPreparedMatchesFull(t, w, b)
			if w.prepared.evidence == nil || w.prepared.evidence.git == nil {
				t.Fatal("Git selection captured no evidence")
			}
			writeFile(t, w.root, "untracked", "edited")
			assertIncremental(t, w, b, "untracked")

			dir := t.TempDir()
			writeFile(t, dir, "excludes", "untracked\n")
			writeFile(t, filepath.Dir(target), filepath.Base(target), "[core]\n\texcludesFile = "+filepath.Join(dir, "excludes")+"\n")
			if err := guard.Verify(); err == nil {
				t.Fatalf("guard accepted a created %s", target)
			}
			writeFile(t, w.root, "untracked", "edited again")
			w.invalidatePath(filepath.Join(w.root, "untracked"), dirtyContent)
			assertPreparedMatchesFull(t, w, b)
		})
	}
}

// Git reports an include only once its target exists.
func TestGitWatchEvidenceCoversDeclaredIncludeTargets(t *testing.T) {
	assertCreatedConfigDetected(t, []absentConfigCase{
		{"include-path", false, func(t *testing.T, _ string, git func(...string)) string {
			target := filepath.Join(t.TempDir(), "absent.gitconfig")
			git("config", "include.path", target)
			return target
		}},
		{"includeif-gitdir", false, func(t *testing.T, root string, git func(...string)) string {
			target := filepath.Join(t.TempDir(), "absent.gitconfig")
			git("config", "includeIf.gitdir:"+root+"/.path", target) // matches this repository
			return target
		}},
		{"relative", false, func(t *testing.T, root string, git func(...string)) string {
			git("config", "include.path", "absent.gitconfig")
			return filepath.Join(root, ".git", "absent.gitconfig")
		}},
		{"relative-from-subdirectory-root", true, func(t *testing.T, root string, git func(...string)) string {
			git("config", "include.path", "absent.gitconfig")
			return filepath.Join(root, ".git", "absent.gitconfig")
		}},
		{"home", false, func(t *testing.T, _ string, git func(...string)) string {
			home := t.TempDir()
			t.Setenv("HOME", home)
			git("config", "include.path", "~/absent.gitconfig")
			return filepath.Join(home, "absent.gitconfig")
		}},
		{"nested", false, func(t *testing.T, _ string, git func(...string)) string {
			dir := t.TempDir()
			writeFile(t, dir, "outer.gitconfig", "[include]\n\tpath = absent.gitconfig\n")
			git("config", "include.path", filepath.Join(dir, "outer.gitconfig"))
			return filepath.Join(dir, "absent.gitconfig")
		}},
	})
}

// Git consults these files by location, and --show-origin lists only files
// that exist and have entries.
func TestGitWatchEvidenceCoversAbsentStandardConfigFiles(t *testing.T) {
	// userConfig lets Git read the per-user files under a temporary HOME.
	userConfig := func(t *testing.T) string {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		unsetenv(t, "GIT_CONFIG_GLOBAL")
		return home
	}
	assertCreatedConfigDetected(t, []absentConfigCase{
		{"home-gitconfig", false, func(t *testing.T, _ string, _ func(...string)) string {
			return filepath.Join(userConfig(t), ".gitconfig")
		}},
		{"xdg-config", false, func(t *testing.T, _ string, _ func(...string)) string {
			userConfig(t)
			xdg := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", xdg)
			return filepath.Join(xdg, "git", "config")
		}},
		{"default-xdg-config", false, func(t *testing.T, _ string, _ func(...string)) string {
			home := userConfig(t)
			unsetenv(t, "XDG_CONFIG_HOME")
			return filepath.Join(home, ".config", "git", "config")
		}},
		{"git-config-global", false, func(t *testing.T, _ string, _ func(...string)) string {
			target := filepath.Join(t.TempDir(), "global.gitconfig")
			t.Setenv("GIT_CONFIG_GLOBAL", target)
			return target
		}},
		{"git-config-system", false, func(t *testing.T, _ string, _ func(...string)) string {
			target := filepath.Join(t.TempDir(), "system.gitconfig")
			t.Setenv("GIT_CONFIG_SYSTEM", target)
			unsetenv(t, "GIT_CONFIG_NOSYSTEM")
			return target
		}},
		{"config-worktree", false, func(t *testing.T, root string, git func(...string)) string {
			git("config", "extensions.worktreeConfig", "true")
			return filepath.Join(root, ".git", "config.worktree")
		}},
	})
}

func unsetenv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "") // restores the original value after the test
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

// Whether an onbranch include applies depends on HEAD, which the evidence
// does not record, so such a configuration keeps full selection.
func TestGitWatchEvidenceFallsBackForBranchConditionalIncludes(t *testing.T) {
	w, b, git := prepareGitWatchFixture(t)
	dir := t.TempDir()
	writeFile(t, dir, "excludes", "untracked\n")
	writeFile(t, dir, "feature.gitconfig", "[core]\n\texcludesFile = "+filepath.Join(dir, "excludes")+"\n")
	git("config", "includeIf.onbranch:feature.path", filepath.Join(dir, "feature.gitconfig"))
	writeFile(t, w.root, "dirty", "dirty")
	assertPreparedMatchesFull(t, w, b)
	git("checkout", "--quiet", "-b", "feature")
	writeFile(t, w.root, "untracked", "edited")
	w.invalidatePath(filepath.Join(w.root, "untracked"), dirtyContent)
	assertPreparedMatchesFull(t, w, b)
}

// Git reads the index through a symbolic link and replaces the link's target
// when it writes, leaving the link itself unchanged.
func TestGitWatchEvidenceFollowsSymlinkedIndex(t *testing.T) {
	w, b, git := prepareGitWatchFixture(t)
	index := filepath.Join(w.root, ".git", "index")
	target := filepath.Join(w.root, ".git", "index.target")
	if err := os.Rename(index, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, index); err != nil {
		t.Fatal(err)
	}
	guard := assertPreparedMatchesFull(t, w, b)
	if w.prepared.evidence == nil || w.prepared.evidence.git == nil {
		t.Fatal("Git selection captured no evidence")
	}
	writeFile(t, w.root, "value", "edited")
	assertIncremental(t, w, b, "value")
	git("add", "-f", "secret") // the untracked file keeps the repository dirty
	if info, err := os.Lstat(index); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("git add replaced the index link: %v", err)
	}
	if err := guard.Verify(); err == nil {
		t.Fatal("guard accepted a changed index behind a symbolic link")
	}
	writeFile(t, w.root, "value", "edited again")
	w.invalidatePath(filepath.Join(w.root, "value"), dirtyContent)
	assertPreparedMatchesFull(t, w, b)
}

// Git reads .gitignore by name, and a case-insensitive filesystem opens
// .GITIGNORE for it. Evidence records ignore files in any case, so an in-place
// edit falls back to full selection even where Git would not read the file.
func TestGitWatchEvidenceRecordsCaseVariantIgnoreFiles(t *testing.T) {
	w, b, _ := prepareGitWatchFixture(t)
	writeFile(t, w.root, "sub/.GITIGNORE", "")
	writeFile(t, w.root, "sub/other", "other")
	guard := assertPreparedMatchesFull(t, w, b)
	if w.prepared.evidence == nil || w.prepared.evidence.git == nil {
		t.Fatal("Git selection captured no evidence")
	}
	name := filepath.Join(w.root, "sub", ".GITIGNORE")
	if _, recorded := w.prepared.evidence.git.contents[name]; !recorded {
		t.Fatalf("evidence does not record %s", name)
	}
	writeFile(t, w.root, "sub/.GITIGNORE", "other\n")
	if err := guard.Verify(); err == nil {
		t.Fatal("guard accepted an edited case-variant ignore file")
	}
	writeFile(t, w.root, "value", "edited")
	w.invalidatePath(filepath.Join(w.root, "value"), dirtyContent)
	assertPreparedMatchesFull(t, w, b)
}

// prepareLinkedWorktreeFixture adds linked worktrees one and two of the Git
// fixture at its commit, each holding an ignored secret, and returns a watch
// on one, the shared Git directory and two.
func prepareLinkedWorktreeFixture(t *testing.T) (*Watch, *Builder, string, string) {
	t.Helper()
	main, b, git := prepareGitWatchFixture(t)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	one, two := filepath.Join(parent, "one"), filepath.Join(parent, "two")
	git("worktree", "add", "--quiet", "-b", "one", one)
	git("worktree", "add", "--quiet", "-b", "two", two)
	writeFile(t, one, "secret", "private")
	writeFile(t, two, "secret", "private")
	writeFile(t, one, "dirty", "dirty") // untracked whichever index Git reads
	info, err := os.Lstat(one)
	if err != nil {
		t.Fatal(err)
	}
	w := &Watch{root: one, identity: info, Changed: make(chan struct{}, 1)}
	return w, b, filepath.Join(main.root, ".git"), two
}

func trackSecret(t *testing.T, worktree string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", worktree, "add", "-f", "secret").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
}

// A linked worktree's .git file names the directory holding its index, and
// that directory's commondir file names the shared config and exclude file.
// Rewriting either in place changes what Git reads with no event and no
// directory stamp change.
func TestGitWatchEvidenceCoversLinkedWorktreeLocation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rewrite func(t *testing.T, one, metadata string)
	}{
		{"gitfile", func(t *testing.T, one, metadata string) {
			// The other worktree's index tracks secret.
			writeFile(t, one, ".git", "gitdir: "+filepath.Join(metadata, "worktrees", "two")+"\n")
		}},
		{"commondir", func(t *testing.T, _, metadata string) {
			// The same directory, named differently.
			writeFile(t, filepath.Join(metadata, "worktrees", "one"), "commondir", metadata+"\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, b, metadata, two := prepareLinkedWorktreeFixture(t)
			trackSecret(t, two)
			guard := assertPreparedMatchesFull(t, w, b)
			if w.prepared.evidence == nil || w.prepared.evidence.git == nil {
				t.Fatal("Git selection captured no evidence")
			}
			tc.rewrite(t, w.root, metadata)
			if err := guard.Verify(); err == nil {
				t.Fatalf("guard accepted a rewritten %s", tc.name)
			}
			writeFile(t, w.root, "value", "edited")
			w.invalidatePath(filepath.Join(w.root, "value"), dirtyContent)
			assertPreparedMatchesFull(t, w, b)
		})
	}
}

// Git resolves the index through the .git file before capture records it. A
// rewrite in between that leaves selection unchanged must not leave the
// evidence stamping the index Git no longer reads.
func TestGitWatchEvidenceCoversGitfileRewrittenDuringCapture(t *testing.T) {
	w, b, metadata, two := prepareLinkedWorktreeFixture(t)
	changeDuringGitCapture(t, nil, func() {
		writeFile(t, w.root, ".git", "gitdir: "+filepath.Join(metadata, "worktrees", "two")+"\n")
	})
	assertPreparedMatchesFull(t, w, b)
	assertGitCaptureHooksRan(t)
	trackSecret(t, two)
	writeFile(t, w.root, "value", "edited")
	w.invalidatePath(filepath.Join(w.root, "value"), dirtyContent)
	assertPreparedMatchesFull(t, w, b)
}

// changeDuringGitCapture runs each non-nil change once: before capture's first
// Git queries, and after them but before capture records the sources they read.
func changeDuringGitCapture(t *testing.T, beforeQueries, afterQueries func()) {
	t.Helper()
	once := func(hook *func(), change func()) {
		if change == nil {
			return
		}
		t.Cleanup(func() { *hook = nil })
		*hook = func() {
			*hook = nil
			change()
		}
	}
	once(&testHookBeforeGitQueries, beforeQueries)
	once(&testHookBeforeRecordingGitSources, afterQueries)
}

func assertGitCaptureHooksRan(t *testing.T) {
	t.Helper()
	if testHookBeforeGitQueries != nil || testHookBeforeRecordingGitSources != nil {
		t.Fatal("preparation did not capture Git evidence")
	}
}

// Full selection's guard compares every ignore rule, so a rule must come and
// go during capture to leave selection unchanged: added after selection, when
// Git lists the directories it excludes, and removed before capture records
// the rules. The evidence records the rules without it, so it must stamp the
// directory it excluded: a file created there later is selected with no event.
func TestGitWatchEvidenceCoversDirectoryUnexcludedDuringCapture(t *testing.T) {
	for _, tc := range []struct{ name, source, kept, dir string }{
		{"gitignore", ".gitignore", "secret\nbuild/\n*.log\n", "empty"},
		{"nested-gitignore", "sub/.gitignore", "", "sub/empty"},
		{"info-exclude", ".git/info/exclude", "", "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, b, _ := prepareGitWatchFixture(t)
			writeFile(t, w.root, tc.source, tc.kept)
			if err := os.Mkdir(filepath.Join(w.root, filepath.FromSlash(tc.dir)), 0o755); err != nil {
				t.Fatal(err)
			}
			changeDuringGitCapture(t,
				func() { writeFile(t, w.root, tc.source, tc.kept+"empty/\n") },
				func() { writeFile(t, w.root, tc.source, tc.kept) })
			assertPreparedMatchesFull(t, w, b)
			assertGitCaptureHooksRan(t)
			writeFile(t, w.root, tc.dir+"/new", "new")
			writeFile(t, w.root, "value", "edited")
			w.invalidatePath(filepath.Join(w.root, "value"), dirtyContent)
			assertPreparedMatchesFull(t, w, b)
		})
	}
}

// Configuration changed during capture is recorded without the files it
// newly makes Git read. Creating them later changes selection with no event.
func TestGitWatchEvidenceCoversConfigChangedDuringCapture(t *testing.T) {
	for _, tc := range []struct {
		name    string
		declare func(git func(...string), dir string)
		write   func(t *testing.T, dir string)
	}{
		{"include-path", func(git func(...string), dir string) {
			git("config", "include.path", filepath.Join(dir, "included"))
		}, func(t *testing.T, dir string) {
			writeFile(t, dir, "included", "[core]\n\texcludesFile = "+filepath.Join(dir, "excludes")+"\n")
		}},
		{"excludes-file", func(git func(...string), dir string) {
			git("config", "core.excludesFile", filepath.Join(dir, "excludes"))
		}, func(*testing.T, string) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, b, git := prepareGitWatchFixture(t)
			dir := t.TempDir()
			changeDuringGitCapture(t, nil, func() { tc.declare(git, dir) })
			assertPreparedMatchesFull(t, w, b)
			assertGitCaptureHooksRan(t)
			writeFile(t, dir, "excludes", "untracked\n")
			tc.write(t, dir)
			writeFile(t, w.root, "value", "edited")
			w.invalidatePath(filepath.Join(w.root, "value"), dirtyContent)
			assertPreparedMatchesFull(t, w, b)
		})
	}
}

// Git reads its default system config, whose location depends on how Git was
// built, unless GIT_CONFIG_NOSYSTEM is set. The default file is only looked
// up here, never created.
func TestGitWatchEvidenceRecordsSystemConfigUnlessDisabled(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		unsetenv(t, "GIT_CONFIG_SYSTEM")
		unsetenv(t, "GIT_CONFIG_NOSYSTEM")
		out, err := exec.Command("git", "var", "GIT_CONFIG_SYSTEM").Output()
		if err != nil {
			t.Skipf("git var GIT_CONFIG_SYSTEM (Git 2.42 and later): %v", err)
		}
		system := strings.TrimSuffix(string(out), "\n")
		w, b, _ := prepareGitWatchFixture(t)
		assertPreparedMatchesFull(t, w, b)
		if w.prepared.evidence == nil || w.prepared.evidence.git == nil {
			t.Fatal("Git selection captured no evidence")
		}
		if _, recorded := w.prepared.evidence.git.contents[system]; !recorded {
			t.Fatalf("evidence does not record the system config %s", system)
		}
	})
	t.Run("nosystem", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "system.gitconfig")
		t.Setenv("GIT_CONFIG_SYSTEM", target)
		t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
		w, b, _ := prepareGitWatchFixture(t)
		writeFile(t, w.root, "dirty", "dirty")
		guard := assertPreparedMatchesFull(t, w, b)
		if w.prepared.evidence == nil || w.prepared.evidence.git == nil {
			t.Fatal("Git selection captured no evidence")
		}
		if _, recorded := w.prepared.evidence.git.contents[target]; recorded {
			t.Fatalf("evidence records %s, which Git does not read", target)
		}
		// Git ignores the file, so creating it leaves selection unchanged.
		dir := t.TempDir()
		writeFile(t, dir, "excludes", "untracked\n")
		writeFile(t, filepath.Dir(target), filepath.Base(target), "[core]\n\texcludesFile = "+filepath.Join(dir, "excludes")+"\n")
		if err := guard.Verify(); err != nil {
			t.Fatalf("an ignored system config rejected selection: %v", err)
		}
		writeFile(t, w.root, "untracked", "edited")
		assertIncremental(t, w, b, "untracked")
	})
}

// The system path comes from Git. Failing to learn it keeps full selection,
// and a GIT_CONFIG_NOSYSTEM that Git reads as true skips it.
func TestResolveGitConfigPathsQueriesSystemPathUnlessDisabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	unsetenv(t, "XDG_CONFIG_HOME")
	unsetenv(t, "GIT_CONFIG_GLOBAL")
	system := filepath.Join(t.TempDir(), "gitconfig")
	global := []string{filepath.Join(home, ".gitconfig"), filepath.Join(home, ".config", "git", "config")}
	withSystem := append([]string{system}, global...)
	const unset = "\x00"
	for _, tc := range []struct {
		name     string
		nosystem string
		output   string
		err      error
		query    bool
		want     []string // nil: no evidence
	}{
		{"default", unset, system + "\n", nil, true, withSystem},
		{"nosystem-empty", "", system + "\n", nil, true, withSystem},
		{"nosystem-false", "False", system + "\n", nil, true, withSystem},
		{"nosystem-zero", "0", system + "\n", nil, true, withSystem},
		{"nosystem", "1", "", nil, false, global},
		{"nosystem-on", "On", "", nil, false, global},
		{"nosystem-integer", "2", "", nil, false, global},
		// Git reads " 1" as true and prints nothing, exiting 1.
		{"nosystem-only-git-reads-as-true", " 1", "", errors.New("exit status 1"), true, nil},
		{"older-git", unset, "", errors.New("exit status 129"), true, nil},
		{"no-output", unset, "", nil, true, nil},
		{"empty-path", unset, "\n", nil, true, nil},
		{"relative-path", unset, "etc/gitconfig\n", nil, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.nosystem == unset {
				unsetenv(t, "GIT_CONFIG_NOSYSTEM")
			} else {
				t.Setenv("GIT_CONFIG_NOSYSTEM", tc.nosystem)
			}
			queried := false
			got, ok := resolveGitConfigPaths(func() ([]byte, error) {
				queried = true
				return []byte(tc.output), tc.err
			})
			if queried != tc.query {
				t.Fatalf("queried system path = %t, want %t", queried, tc.query)
			}
			if ok != (tc.want != nil) || !slices.Equal(got, tc.want) {
				t.Fatalf("paths = %q, %t; want %q", got, ok, tc.want)
			}
		})
	}
}

// Git also reports the global paths, but one git var reports one variable.
// The evidence resolves them itself; check that it agrees with Git.
func TestStandardGitConfigPathsMatchGit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"default", func(t *testing.T) {
			unsetenv(t, "XDG_CONFIG_HOME")
			unsetenv(t, "GIT_CONFIG_GLOBAL")
		}},
		{"xdg-config-home", func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			unsetenv(t, "GIT_CONFIG_GLOBAL")
		}},
		{"git-config-global", func(t *testing.T) {
			t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global.gitconfig"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "system.gitconfig"))
			unsetenv(t, "GIT_CONFIG_NOSYSTEM")
			tc.setup(t)
			root := t.TempDir()
			var want []string
			for _, variable := range []string{"GIT_CONFIG_SYSTEM", "GIT_CONFIG_GLOBAL"} {
				out, err := exec.Command("git", "-C", root, "var", variable).Output()
				if err != nil {
					t.Skipf("git var %s (Git 2.42 and later): %v", variable, err)
				}
				want = append(want, strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")...)
			}
			got, ok := standardGitConfigPaths(root)
			if !ok {
				t.Fatal("config paths were not resolved")
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("paths = %q, Git reads %q", got, want)
			}
		})
	}
}

// Git's origin listing omits a repository config with no entries, so capture
// records its path directly. Adding an excludes file there later must reach
// the evidence even while the repository stays dirty.
func TestGitWatchEvidenceRecordsRepositoryConfigWithoutEntries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(config string) error
	}{
		{"empty", func(config string) error { return os.WriteFile(config, nil, 0o644) }},
		{"absent", os.Remove},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, b, _ := prepareGitWatchFixture(t)
			config := filepath.Join(w.root, ".git", "config")
			if err := tc.setup(config); err != nil {
				t.Fatal(err)
			}
			writeFile(t, w.root, "value", "dirty")
			assertPreparedMatchesFull(t, w, b)
			excludes := filepath.Join(t.TempDir(), "excludes")
			writeFile(t, filepath.Dir(excludes), "excludes", "untracked\n")
			writeFile(t, w.root, ".git/config", "[core]\n\texcludesFile = "+excludes+"\n")
			writeFile(t, w.root, "value", "edited")
			w.invalidatePath(filepath.Join(w.root, "value"), dirtyContent)
			assertPreparedMatchesFull(t, w, b)
		})
	}
}

// An ignored .gitignore created after the walk passed its directory is in
// the directory's listing without recorded contents, and selection does not
// show it while it adds no rules. Rules later written into it in place must
// not let a hinted edit reuse the selection.
func TestGitWatchEvidenceRejectsIgnoreFileCreatedDuringCapture(t *testing.T) {
	w, b, _ := prepareGitWatchFixture(t)
	writeFile(t, w.root, ".git/info/exclude", "sub/.gitignore\n")
	writeFile(t, w.root, "sub/extra", "extra")
	writeFile(t, w.root, "value", "dirty")
	t.Cleanup(func() { testHookBeforeDirectoryStamps = nil })
	testHookBeforeDirectoryStamps = func() {
		testHookBeforeDirectoryStamps = nil
		writeFile(t, w.root, "sub/.gitignore", "")
	}
	assertPreparedMatchesFull(t, w, b)
	if testHookBeforeDirectoryStamps != nil {
		t.Fatal("preparation did not capture evidence")
	}
	writeFile(t, w.root, "sub/.gitignore", "extra\n")
	writeFile(t, w.root, "value", "edited")
	w.invalidatePath(filepath.Join(w.root, "value"), dirtyContent)
	assertPreparedMatchesFull(t, w, b)
}
