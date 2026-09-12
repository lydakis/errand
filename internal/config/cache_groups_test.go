package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func cacheGroupWorkspace(t *testing.T, input string, dirs ...string) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".errand.toml"), []byte("[workspace]\nroot = true\n"+input), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func resolveCacheGroups(t *testing.T, root string, opts RunOverrides) EffectiveRun {
	t.Helper()
	opts.URL = "http://runner.invalid"
	got, err := ResolveRun(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCacheGroupsDiscoverColdDirectoriesAndKeepIdentities(t *testing.T) {
	root := cacheGroupWorkspace(t, `[caches]
 compiler = "target"
 [caches.dependencies]
 roots = [".", "packages/*", "packages/core"]
 path = "node_modules"
 [caches.builds]
 roots = ["packages/*"]
 path = "dist"
 `, "packages/core", "packages/ui")
	first := resolveCacheGroups(t, filepath.Join(root, "packages/core"), RunOverrides{})
	byPath := map[string]string{}
	for _, b := range first.Caches {
		byPath[b.Path] = b.Name
	}
	if len(byPath) != 6 || byPath["target"] != "compiler" {
		t.Fatalf("bindings: %+v", first.Caches)
	}
	for _, p := range []string{"node_modules", "packages/core/node_modules", "packages/ui/node_modules", "packages/core/dist", "packages/ui/dist"} {
		if byPath[p] == "" {
			t.Fatalf("missing %s: %+v", p, first.Caches)
		}
		if _, err := os.Stat(filepath.Join(root, p)); !os.IsNotExist(err) {
			t.Fatalf("inspection created %s: %v", p, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "packages/added"), 0700); err != nil {
		t.Fatal(err)
	}
	second := resolveCacheGroups(t, root, RunOverrides{})
	if len(second.Caches) != 8 {
		t.Fatal(second.Caches)
	}
	for _, b := range second.Caches {
		if old := byPath[b.Path]; old != "" && old != b.Name {
			t.Fatalf("identity changed: %s", b.Path)
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, ".errand.toml"))
	if err != nil {
		t.Fatal(err)
	}
	reordered := strings.Replace(string(raw), `[".", "packages/*", "packages/core"]`, `["packages/core", "packages/*", "."]`, 1)
	other := cacheGroupWorkspace(t, strings.TrimPrefix(reordered, "[workspace]\nroot = true\n"), "packages/added", "packages/ui", "packages/core")
	third := resolveCacheGroups(t, other, RunOverrides{})
	if !reflect.DeepEqual(second.Caches, third.Caches) {
		t.Fatal("identities or ordering depend on checkout location or pattern order")
	}
}

func TestCacheGroupPrecedenceBeforeExpansion(t *testing.T) {
	root := cacheGroupWorkspace(t, `[caches.missing]
 roots = ["missing/*"]
 path = "dist"
 [profiles.clean.caches]
 [profiles.build.caches.dependencies]
 roots = ["packages/*"]
 path = "node_modules"
 `, "packages/core")
	for _, opts := range []RunOverrides{{Caches: []proto.CacheBinding{}}, {Profile: "clean"}} {
		got := resolveCacheGroups(t, root, opts)
		if len(got.Caches) != 0 || !got.CachesOverride {
			t.Fatal(got)
		}
	}
	got := resolveCacheGroups(t, root, RunOverrides{Profile: "build"})
	if len(got.Caches) != 1 || got.Caches[0].Path != "packages/core/node_modules" || !got.CachesOverride {
		t.Fatal(got)
	}
	_, err := ResolveRun(root, RunOverrides{URL: "http://runner.invalid"})
	if err == nil || !strings.Contains(err.Error(), "missing/*") || !strings.Contains(err.Error(), ".errand.toml") {
		t.Fatalf("missing diagnostic: %v", err)
	}
}

func TestCacheGroupValidation(t *testing.T) {
	for _, input := range []string{
		`a = {roots = [], path = "dist"}`,
		`a = {roots = ["../outside"], path = "dist"}`,
		`a = {roots = ["/tmp"], path = "dist"}`,
		`a = {roots = ["packages/**"], path = "dist"}`,
		`a = {roots = ["packages/["], path = "dist"}`,
		`a = {roots = ["."], path = "../out"}`,
		`a = {roots = ["."], path = "*"}`,
		`a = {roots = ["."], path = "dist", typo = true}`,
		`a = {roots = ["."], path = "dist"}
b = "dist/nested"`,
		`a = {roots = ["."], path = "dist"}
b = {roots = ["."], path = "DIST"}`,
	} {
		t.Run(input, func(t *testing.T) {
			root := cacheGroupWorkspace(t, "[caches]\n"+input)
			if _, err := ResolveRun(root, RunOverrides{URL: "http://runner.invalid"}); err == nil {
				t.Fatal("accepted invalid group")
			}
		})
	}
	t.Run("limit", func(t *testing.T) {
		var dirs []string
		for i := 0; i < 65; i++ {
			dirs = append(dirs, fmt.Sprintf("packages/p%d", i))
		}
		root := cacheGroupWorkspace(t, `[caches.a]
roots = ["packages/*"]
path = "dist"`, dirs...)
		if _, err := ResolveRun(root, RunOverrides{URL: "http://runner.invalid"}); err == nil || !strings.Contains(err.Error(), "64") {
			t.Fatalf("limit: %v", err)
		}
	})
}

func TestCacheGroupsDoNotFollowSymlinkRoots(t *testing.T) {
	root := cacheGroupWorkspace(t, `[caches.a]
roots = ["packages/*"]
path = "dist"`, "packages/core")
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, "victim"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "packages/README"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "packages/linked")); err != nil {
		t.Fatal(err)
	}
	got := resolveCacheGroups(t, root, RunOverrides{})
	if len(got.Caches) != 1 || got.Caches[0].Path != "packages/core/dist" {
		t.Fatal(got.Caches)
	}
	if err := os.Symlink(outside, filepath.Join(root, "external")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".errand.toml"), []byte("[caches.a]\nroots=['external/*']\npath='dist'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveRun(root, RunOverrides{URL: "http://runner.invalid"}); err == nil {
		t.Fatal("followed symlink parent")
	}
}

func TestPersistentCacheGroupsExpandOnlyWhenUsed(t *testing.T) {
	root := cacheGroupWorkspace(t, `[caches.dependencies]
roots = ["packages/*"]
path = "node_modules"
[profiles.dev.run]
workspace = "dev"
[profiles.explicit.run]
workspace = "dev"
[profiles.explicit.caches.builds]
roots = ["packages/*"]
path = "dist"
`)
	opts := RunOverrides{URL: "http://runner.invalid", Profile: "dev"}
	got, err := ResolveRun(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err = got.PrepareExecution(false); err != nil {
		t.Fatal(err)
	}
	if got.Caches != nil || got.CacheSources != nil {
		t.Fatal("expanded inherited workspace caches")
	}
	if _, err = ResolveWorkspaceCreation(root, opts); err == nil {
		t.Fatal("creation skipped discovery")
	}
	opts.Profile = "explicit"
	if _, err = ResolveRun(root, opts); err == nil {
		t.Fatal("explicit run skipped discovery")
	}
	if _, err = ResolvePush(root, opts); err != nil {
		t.Fatalf("push expanded caches: %v", err)
	}
	if err = os.MkdirAll(filepath.Join(root, "packages/core"), 0700); err != nil {
		t.Fatal(err)
	}
	opts.Profile = "dev"
	got, err = ResolveWorkspaceCreation(root, opts)
	if err != nil || len(got.Caches) != 1 {
		t.Fatalf("creation did not expand: %+v %v", got.Caches, err)
	}
}
