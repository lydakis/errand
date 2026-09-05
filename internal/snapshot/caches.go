package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
)

func withoutCachePaths(root string, paths []string, caches []proto.CacheBinding) ([]string, error) {
	if len(caches) == 0 {
		return paths, nil
	}
	// Git retains index spelling after case-only renames. Compare the filesystem
	// objects for potentially aliased roots rather than case-folding exclusions,
	// which would discard distinct source paths on case-sensitive filesystems.
	cacheInfo := make([]os.FileInfo, len(caches))
	for i, cache := range caches {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(cache.Path)))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		cacheInfo[i] = info
	}
	filtered := paths[:0]
	for _, name := range paths {
		if pathpolicy.InCache(name, caches) {
			continue
		}
		parts := strings.Split(name, "/")
		for i, cache := range caches {
			if cacheInfo[i] == nil {
				continue
			}
			depth := strings.Count(cache.Path, "/") + 1
			if len(parts) < depth {
				continue
			}
			prefix := strings.Join(parts[:depth], "/")
			if !strings.EqualFold(prefix, cache.Path) {
				continue
			}
			info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(prefix)))
			if err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			if info != nil && os.SameFile(info, cacheInfo[i]) {
				return nil, fmt.Errorf("Git path %q has different casing from cache path %q", name, cache.Path)
			}
		}
		filtered = append(filtered, name)
	}
	return filtered, nil
}
