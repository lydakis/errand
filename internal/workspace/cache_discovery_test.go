package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/tomlconfig"
)

func TestCacheGroupsSkipImplicitMetadataAndPreserveHiddenDirectories(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".git", ".errand-change-pending", ".hidden", "package"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	var c struct {
		Caches Caches `toml:"caches"`
	}
	if _, err := tomlconfig.Decode("[caches.deps]\nroots=['*']\npath='node_modules'", &c); err != nil {
		t.Fatal(err)
	}
	bindings, _, err := c.Caches.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 {
		t.Fatal(bindings)
	}
	for _, b := range bindings {
		if b.Path != "package/node_modules" && b.Path != ".hidden/node_modules" {
			t.Fatal(b)
		}
	}
	if _, err := tomlconfig.Decode("[caches.deps]\nroots=['.git']\npath='node_modules'", &c); err == nil {
		t.Fatal("explicit reserved root accepted")
	}
}

func TestCacheGroupDiagnosticsIdentifyDeclaration(t *testing.T) {
	for _, input := range []string{
		"roots=['.']", "roots=['.']\npath='../out'", "roots=['packages/a[/]b']\npath='dist'",
	} {
		var c struct {
			Caches Caches `toml:"caches"`
		}
		if _, err := tomlconfig.Decode("[caches.dependencies]\n"+input, &c); err == nil || !strings.Contains(err.Error(), "dependencies") {
			t.Fatalf("error: %v", err)
		}
	}
}

func TestCacheDiscoveryIntermediateFanoutAndLimit(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 65; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("p%02d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "p00", "selected"), 0700); err != nil {
		t.Fatal(err)
	}
	var c struct {
		Caches Caches `toml:"caches"`
	}
	if _, err := tomlconfig.Decode("[caches.deps]\nroots=['*/selected']\npath='dist'", &c); err != nil {
		t.Fatal(err)
	}
	bindings, _, err := c.Caches.Resolve(root)
	if err != nil || len(bindings) != 1 || bindings[0].Path != "p00/selected/dist" {
		t.Fatalf("valid intermediate fanout: %+v %v", bindings, err)
	}
	if _, err := tomlconfig.Decode("[caches.deps]\nroots=['*']\npath='dist'", &c); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Caches.Resolve(root); err == nil || !strings.Contains(err.Error(), "64") {
		t.Fatalf("limit: %v", err)
	}
}

func TestCacheDiscoveryBudgetCoversUnmatchedEntriesAndMultipleGroups(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 12; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file%d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "known"), 0700); err != nil {
		t.Fatal(err)
	}
	d, err := openCacheDiscovery(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.root.Close()
	// A small budget exercises the same boundary without creating 10,000 files.
	// Nonmatching regular files must count, not just matching directories.
	d.remaining = 5
	if _, err := d.roots([]string{"missing*"}, 64); err == nil || !strings.Contains(err.Error(), "filesystem visits") || !strings.Contains(err.Error(), "missing*") {
		t.Fatalf("unbounded unmatched scan: %v", err)
	}
	d.remaining = 4
	for i := 0; i < 2; i++ {
		if _, err := d.roots([]string{"known"}, 64); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.roots([]string{"known"}, 64); err == nil || !strings.Contains(err.Error(), "filesystem visits") {
		t.Fatalf("budget reset between groups: %v", err)
	}
}

func TestCacheDiscoveryStopsAtFinalBindingLimit(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 8; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("p%d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	d, err := openCacheDiscovery(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.root.Close()
	// Simulate one remaining slot after bindings from earlier declarations.
	d.remaining = 4
	if _, err := d.roots([]string{"*"}, 1); err == nil || !strings.Contains(err.Error(), "named caches") {
		t.Fatalf("did not reject the second final match promptly: %v", err)
	}
}
