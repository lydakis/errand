package snapshot

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// countGitStatus puts a git wrapper first on PATH and returns how many
// status commands have run through it since.
func countGitStatus(t *testing.T) func() int {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "invocations")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\nexec '" + git + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		t.Helper()
		data, err := os.ReadFile(log)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		count := 0
		for line := range strings.Lines(string(data)) {
			for arg := range strings.FieldsSeq(line) {
				if arg == "status" {
					count++
				}
			}
		}
		return count
	}
}

// cleanGitWatchFixture is the Git fixture with its untracked file committed,
// so Git reports no changes.
func cleanGitWatchFixture(t *testing.T) (*Watch, *Builder, func(...string)) {
	t.Helper()
	w, b, git := prepareGitWatchFixture(t)
	git("add", "untracked")
	git("commit", "--quiet", "-m", "clean")
	if gi, err := gitInfo(w.root); err != nil || gi.Dirty {
		t.Fatalf("fixture is not clean: %+v, %v", gi, err)
	}
	return w, b, git
}

// Push sends no repository metadata, so an incremental cycle, including its
// post-freeze guard, never asks Git for status. Outside a repository that
// would also be a failing status and a rev-parse.
func TestWatchIncrementalCycleRunsNoGitStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture func(*testing.T) (*Watch, *Builder)
	}{
		{"explicit", prepareWatchFixture},
		{"git", func(t *testing.T) (*Watch, *Builder) {
			w, b, _ := prepareGitWatchFixture(t)
			return w, b
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, b := tc.fixture(t)
			assertPreparedMatchesFull(t, w, b)
			before := w.prepared.fullAt
			statuses := countGitStatus(t)
			for _, content := range []string{"edited", "edited again"} {
				writeFile(t, w.root, "value", content)
				w.invalidatePath(filepath.Join(w.root, "value"), dirtyContent)
				_, _, guard, err := w.PrepareSnapshot(b)
				if err != nil {
					t.Fatal(err)
				}
				if err := guard.Verify(); err != nil {
					t.Fatal(err)
				}
			}
			if w.prepared.fullAt != before {
				t.Fatal("content-only edit used full selection")
			}
			if n := statuses(); n != 0 {
				t.Fatalf("incremental cycles ran git status %d times", n)
			}
		})
	}
}

// The first edit in a clean worktree changes Git's dirty state but not the
// selection, so it stays an ordinary incremental edit.
func TestGitWatchFirstEditInCleanWorktreeStaysIncremental(t *testing.T) {
	w, b, _ := cleanGitWatchFixture(t)
	guard := assertPreparedMatchesFull(t, w, b)
	writeFile(t, w.root, "value", "edited")
	if err := guard.Verify(); err != nil {
		t.Fatalf("guard rejected a content edit in a clean worktree: %v", err)
	}
	assertIncremental(t, w, b, "value")
}

// Job snapshots report HEAD and dirty state, so their guard rejects a change to
// either. A watch reports neither: without evidence its guard reselects fully
// and still accepts both.
func TestSelectionGuardBindsRepositoryMetadataOnlyForJobs(t *testing.T) {
	w, b, git := cleanGitWatchFixture(t)
	// A branch-conditional include leaves the watch without evidence.
	git("config", "includeIf.onbranch:feature.path", filepath.Join(t.TempDir(), "feature.gitconfig"))
	head := func() string {
		t.Helper()
		out, err := exec.Command("git", "-C", w.root, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	job := func(want GitInfo) *SelectionGuard {
		t.Helper()
		_, gi, _, guard, err := SelectFilesGuarded(w.root, w.opts)
		if err != nil {
			t.Fatal(err)
		}
		if gi != want {
			t.Fatalf("job snapshot reported %+v, want %+v", gi, want)
		}
		return guard
	}
	check := func(before GitInfo, change func()) {
		t.Helper()
		jobGuard := job(before)
		watchGuard := assertPreparedMatchesFull(t, w, b)
		if w.prepared.evidence != nil {
			t.Fatal("watch captured evidence despite a branch-conditional include")
		}
		change()
		if err := jobGuard.Verify(); !IsSourceChanged(err) {
			t.Fatalf("job guard accepted changed repository metadata: %v", err)
		}
		if err := watchGuard.Verify(); err != nil {
			t.Fatalf("watch guard rejected changed repository metadata: %v", err)
		}
	}
	first := head()
	check(GitInfo{Repository: true, Commit: first}, func() { writeFile(t, w.root, "value", "edited") })
	check(GitInfo{Repository: true, Commit: first, Dirty: true}, func() { git("commit", "--quiet", "-a", "-m", "edit") })
	if head() == first {
		t.Fatal("commit did not move HEAD")
	}
	job(GitInfo{Repository: true, Commit: head()})
}
