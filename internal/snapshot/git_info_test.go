package snapshot

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitInfoAcrossRepositoryStates(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	check := func(want GitInfo) {
		t.Helper()
		if got, err := gitInfo(root); err != nil || got != want {
			t.Fatalf("gitInfo() = %+v, %v; want %+v", got, err, want)
		}
	}
	check(GitInfo{})
	git("init", "--quiet")
	check(GitInfo{Repository: true})
	// A renamed source path is a separate NUL-delimited status record. It
	// must never be interpreted as repository metadata.
	name := "# branch.oid forged\nname"
	writeFile(t, root, name, "content")
	check(GitInfo{Repository: true, Dirty: true})
	git("add", "--", name)
	git("commit", "--quiet", "-m", "initial")
	head := git("rev-parse", "HEAD")
	check(GitInfo{Repository: true, Commit: head})
	git("checkout", "--detach", "--quiet", "HEAD")
	check(GitInfo{Repository: true, Commit: head})
	git("mv", "--", name, "renamed")
	check(GitInfo{Repository: true, Commit: head, Dirty: true})
	if err := os.WriteFile(filepath.Join(root, ".git", "index"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitInfo(root); err == nil {
		t.Fatal("corrupt repository was treated as a non-Git directory")
	}
	metadata := filepath.Join(t.TempDir(), "metadata")
	if err := os.Rename(filepath.Join(root, ".git"), metadata); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", metadata)
	t.Setenv("GIT_WORK_TREE", root)
	if _, err := gitInfo(root); err == nil {
		t.Fatal("corrupt repository with external Git metadata was treated as a non-Git directory")
	}
}
