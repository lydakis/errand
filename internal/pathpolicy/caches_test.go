package pathpolicy

import (
	"github.com/lydakis/errand/internal/proto"
	"os"
	"path/filepath"
	"testing"
)

func TestCacheBindingBoundaries(t *testing.T) {
	for _, paths := range [][]proto.CacheBinding{
		{{Name: "build", Path: "target"}},
		{{Name: "go", Path: ".cache/go"}, {Name: "rust", Path: "target"}},
		{{Name: "a", Path: "Build/one"}, {Name: "b", Path: "Build/two"}},
	} {
		if err := ValidateCaches(paths); err != nil {
			t.Fatal(err)
		}
	}
	for _, paths := range [][]proto.CacheBinding{
		{{Name: "../build", Path: "target"}}, {{Name: "build", Path: "../target"}},
		{{Name: "build", Path: ".git/objects"}}, {{Name: "build", Path: "."}},
		{{Name: "build", Path: "out"}, {Name: "other", Path: "out/child"}},
		{{Name: "build", Path: "out"}, {Name: "build", Path: "other"}},
		{{Name: "a", Path: "out"}, {Name: "b", Path: "OUT"}},
		{{Name: "a", Path: "Build/one"}, {Name: "b", Path: "build/two"}},
		{{Name: "a", Path: ".cache/Build/one"}, {Name: "b", Path: ".cache/build/two"}},
	} {
		if err := ValidateCaches(paths); err == nil {
			t.Fatalf("accepted %+v", paths)
		}
	}
}

func TestCacheExclusionUsesExactPathCase(t *testing.T) {
	caches := []proto.CacheBinding{{Name: "compiler", Path: "build"}}
	if InCache("Build/source.go", caches) {
		t.Fatal("excluded a distinct source directory")
	}
	if !InCache("build/object", caches) {
		t.Fatal("did not exclude cache contents")
	}
}

func TestCacheCasingMatchesFilesystem(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Build"), 0700); err != nil {
		t.Fatal(err)
	}
	_, statErr := os.Lstat(filepath.Join(root, "build"))
	if statErr != nil && !os.IsNotExist(statErr) {
		t.Fatal(statErr)
	}
	for _, cachePath := range []string{"build", "build/cache"} {
		err := ValidateCacheCasing(root, []proto.CacheBinding{{Name: "compiler", Path: cachePath}})
		if statErr == nil && err == nil {
			t.Fatalf("accepted ambiguous casing: %s", cachePath)
		}
		if os.IsNotExist(statErr) && err != nil {
			t.Fatalf("rejected distinct path: %v", err)
		}
	}
	if err := ValidateCacheCasing(root, []proto.CacheBinding{{Name: "compiler", Path: "Build/cache"}}); err != nil {
		t.Fatal(err)
	}
}
