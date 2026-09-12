package workspace

import (
	"crypto/sha256"
	"fmt"
	"path"
	"slices"
	"sort"

	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
)

// Resolve expands only the selected config layer. It never creates cache
// directories or requires them to exist. Sources maps binding names to declarations.
func (c Caches) Resolve(root string) ([]proto.CacheBinding, map[string]string, error) {
	if c == nil {
		return nil, nil, nil
	}
	bindings := make([]proto.CacheBinding, 0, len(c))
	sources := map[string]string{}
	var names []string
	for name := range c {
		names = append(names, name)
	}
	slices.Sort(names)
	var discovery *cacheDiscovery
	for _, name := range names {
		entry := c[name]
		if entry.Roots == nil {
			bindings = append(bindings, proto.CacheBinding{Name: name, Path: entry.Path})
			sources[name] = "caches." + name
		} else {
			if discovery == nil {
				var err error
				discovery, err = openCacheDiscovery(root)
				if err != nil {
					return nil, nil, fmt.Errorf("cache group %q: %w", name, err)
				}
				defer discovery.root.Close()
			}
			roots, err := discovery.roots(entry.Roots, pathpolicy.MaxCaches-len(bindings))
			if err != nil {
				return nil, nil, fmt.Errorf("cache group %q: %w", name, err)
			}
			for _, dir := range roots {
				destination := path.Join(dir, entry.Path)
				bindingName := cacheGroupName(name, destination)
				bindings = append(bindings, proto.CacheBinding{Name: bindingName, Path: destination})
				sources[bindingName] = "caches." + name
			}
		}
		if len(bindings) > pathpolicy.MaxCaches {
			return nil, nil, fmt.Errorf("at most %d named caches may be bound", pathpolicy.MaxCaches)
		}
	}
	if err := pathpolicy.ValidateCaches(bindings); err != nil {
		return nil, nil, err
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Name < bindings[j].Name })
	return bindings, sources, nil
}

func cacheGroupName(group, destination string) string {
	digest := sha256.Sum256([]byte(group + "\x00" + destination))
	prefix := group
	if len(prefix) > 31 {
		prefix = prefix[:31]
	}
	return fmt.Sprintf("%s-%x", prefix, digest[:16])
}
