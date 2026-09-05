package pathpolicy

import (
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

// ValidateCaches requires distinct names and non-overlapping directory paths.
// Case-insensitive overlap is refused so bindings work on both target platforms.
func ValidateCaches(caches []proto.CacheBinding) error {
	if len(caches) > 64 {
		return fmt.Errorf("at most 64 named caches may be bound")
	}
	names := map[string]bool{}
	paths := []string{}
	for _, cache := range caches {
		name := cache.Name
		if name == "" || len(name) > 64 || name == "." || name == ".." || names[name] {
			return fmt.Errorf("invalid or repeated cache name %q", name)
		}
		for _, c := range name {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
				return fmt.Errorf("invalid cache name %q", name)
			}
		}
		names[name] = true
		paths = append(paths, cache.Path)
		if cache.Path == ".errand.toml" || cache.Path == ".errandignore" {
			return fmt.Errorf("cache path %q uses configuration metadata", cache.Path)
		}
	}
	if err := ValidateArtifacts(paths); err != nil {
		return fmt.Errorf("cache paths: %w", err)
	}
	// Binding creates missing parents, which can appear in retained results.
	// Those results must be portable to case-insensitive filesystems, too.
	parents := map[string]string{}
	for _, name := range paths {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			folded := strings.ToLower(parent)
			if previous, exists := parents[folded]; exists && previous != parent {
				return fmt.Errorf("cache parent paths %q and %q have conflicting casing", previous, parent)
			}
			parents[folded] = parent
		}
	}
	for i, a := range paths {
		for _, b := range paths[:i] {
			if beneath(strings.ToLower(a), strings.ToLower(b)) || beneath(strings.ToLower(b), strings.ToLower(a)) {
				return fmt.Errorf("cache paths %q and %q overlap", a, b)
			}
		}
	}
	return nil
}

// InCache excludes bindings and their descendants, even if artifacts select them.
func InCache(name string, caches []proto.CacheBinding) bool {
	for _, cache := range caches {
		if beneath(name, cache.Path) {
			return true
		}
	}
	return false
}

func beneath(name, root string) bool { return name == root || strings.HasPrefix(name, root+"/") }

// ValidateCacheCasing refuses paths resolved through differently cased entries
// on case-insensitive filesystems. Exact matching must never silently exclude a
// distinct source directory on a case-sensitive filesystem.
func ValidateCacheCasing(workspace string, caches []proto.CacheBinding) error {
	if len(caches) == 0 {
		return nil
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, cache := range caches {
		parent := "."
		parts := strings.Split(cache.Path, "/")
		for i, part := range parts {
			current := path.Join(parent, part)
			info, err := root.Lstat(current)
			if os.IsNotExist(err) {
				break
			}
			if err != nil {
				return err
			}
			dir, err := root.Open(parent)
			if err != nil {
				return err
			}
			entries, readErr := dir.ReadDir(-1)
			closeErr := dir.Close()
			if readErr != nil {
				return readErr
			}
			if closeErr != nil {
				return closeErr
			}
			exact := false
			for _, entry := range entries {
				if entry.Name() == part {
					exact = true
					break
				}
			}
			if !exact {
				return fmt.Errorf("cache path %q has different casing from existing path %q", cache.Path, current)
			}
			if i < len(parts)-1 && !info.IsDir() {
				return fmt.Errorf("cache parent %q must be a directory", current)
			}
			parent = current
		}
	}
	return nil
}
