package snapshot

import (
	"os"
	"os/exec"
	"path/filepath"
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
