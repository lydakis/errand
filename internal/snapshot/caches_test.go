package snapshot

import (
	"github.com/lydakis/errand/internal/proto"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCacheSelectionSkipsUnreadableContentsAndGuardChanges(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(root, "target")
	if err := os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cache, 0700) })
	if err := os.Chmod(cache, 0); err != nil {
		t.Fatal(err)
	}
	paths, _, _, guard, err := SelectFilesGuarded(root, SelectOptions{Caches: []proto.CacheBinding{{Name: "compiler", Path: "target"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range paths {
		if name == "target" {
			t.Fatal("cache selected")
		}
	}
	if err := os.Chmod(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "new-object"), []byte("local build"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := guard.Verify(); err != nil {
		t.Fatalf("cache changes invalidated source selection: %v", err)
	}
}

func TestCacheSelectionPreservesDistinctPathCase(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "Build"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Build", "source.go"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	_, statErr := os.Lstat(filepath.Join(root, "build"))
	if statErr != nil && !os.IsNotExist(statErr) {
		t.Fatal(statErr)
	}
	paths, _, _, err := SelectFilesWithOptions(root, SelectOptions{Caches: []proto.CacheBinding{{Name: "compiler", Path: "build"}}})
	if statErr == nil {
		if err == nil {
			t.Fatal("accepted ambiguous cache casing")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(paths, "Build/source.go") {
		t.Fatalf("source excluded: %v", paths)
	}
}

func cacheTestGit(t *testing.T, root string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git: %v %s", err, out)
	}
}

func cacheTestWrite(t *testing.T, root, name, body string) {
	t.Helper()
	full := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCacheGitPolicyIgnoresCacheChanges(t *testing.T) {
	for _, subdir := range []string{"", "project"} {
		t.Run(subdir, func(t *testing.T) {
			repo := t.TempDir()
			cacheTestGit(t, repo, "init", "--quiet")
			root := filepath.Join(repo, subdir)
			cacheTestWrite(t, root, "source.txt", "source")
			cacheTestWrite(t, root, ".gitignore", "target/\n")
			cacheTestWrite(t, root, "target/.gitignore", "old\n")
			_, _, _, guard, err := SelectFilesGuarded(root, SelectOptions{Caches: []proto.CacheBinding{{Name: "compiler", Path: "target"}}})
			if err != nil {
				t.Fatal(err)
			}
			cacheTestWrite(t, root, "target/.gitignore", "new\n")
			if err := guard.Verify(); err != nil {
				t.Fatalf("cache change invalidated snapshot: %v", err)
			}
			cacheTestWrite(t, root, ".gitignore", "target/\nother/\n")
			if err := guard.Verify(); err == nil {
				t.Fatal("source policy change was ignored")
			}
		})
	}
}

func TestCacheGitIndexCasing(t *testing.T) {
	root := t.TempDir()
	cacheTestGit(t, root, "init", "--quiet")
	cacheTestWrite(t, root, "Build/object.txt", "object")
	cacheTestGit(t, root, "add", "Build/object.txt")
	if err := os.Rename(filepath.Join(root, "Build"), filepath.Join(root, "build")); err != nil {
		t.Fatal(err)
	}
	_, statErr := os.Lstat(filepath.Join(root, "Build"))
	if statErr != nil && !os.IsNotExist(statErr) {
		t.Fatal(statErr)
	}
	// On a case-sensitive filesystem, restore a distinct source directory.
	if os.IsNotExist(statErr) {
		cacheTestWrite(t, root, "Build/object.txt", "source")
	}
	files, _, _, err := SelectFilesWithOptions(root, SelectOptions{Caches: []proto.CacheBinding{{Name: "compiler", Path: "build"}}})
	if statErr == nil {
		if err == nil || !strings.Contains(err.Error(), "casing") {
			t.Fatalf("ambiguous index casing: %v %v", files, err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(files, "Build/object.txt") || slices.Contains(files, "build/object.txt") {
		t.Fatalf("distinct source/cache paths: %v", files)
	}
}
