package workspace

import (
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/lydakis/errand/internal/pathpolicy"
)

// The shared work budget bounds intermediate fanout and nonmatching entries,
// independently of the final binding limit. It applies across all groups.
const maxCacheDiscoveryWork = 10_000

type cacheDiscovery struct {
	root      *os.Root
	remaining int
}

func openCacheDiscovery(workspace string) (*cacheDiscovery, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	return &cacheDiscovery{root: root, remaining: maxCacheDiscoveryWork}, nil
}

func (d *cacheDiscovery) spend() error {
	if d.remaining == 0 {
		return fmt.Errorf("directory discovery exceeds %d filesystem visits; narrow the roots patterns", maxCacheDiscoveryWork)
	}
	d.remaining--
	return nil
}

func (d *cacheDiscovery) roots(patterns []string, limit int) ([]string, error) {
	found := map[string]bool{}
	for _, pattern := range patterns {
		matched := false
		accept := func(dir string) error {
			matched = true
			found[dir] = true
			if len(found) > limit {
				return fmt.Errorf("at most %d named caches may be bound", pathpolicy.MaxCaches)
			}
			return nil
		}
		var err error
		if pattern == "." {
			err = d.spend()
			if err == nil {
				err = accept(".")
			}
		} else {
			matches := []string{"."}
			components := strings.Split(pattern, "/")
			for i, component := range components {
				var next []string
				for _, parent := range matches {
					err = d.children(parent, component, func(dir string) error {
						if i == len(components)-1 {
							return accept(dir)
						}
						next = append(next, dir)
						return nil
					})
					if err != nil {
						break
					}
				}
				if err != nil {
					break
				}
				matches = next
			}
		}
		if err != nil {
			return nil, fmt.Errorf("root pattern %q: %w", pattern, err)
		}
		if !matched {
			return nil, fmt.Errorf("root pattern %q matched no directories", pattern)
		}
	}
	roots := make([]string, 0, len(found))
	for dir := range found {
		roots = append(roots, dir)
	}
	slices.Sort(roots)
	return roots, nil
}

func (d *cacheDiscovery) children(parent, component string, visit func(string) error) error {
	// Recheck intermediate parents before opening; os.Root also confines all
	// operations if the source tree is concurrently replaced.
	if err := d.spend(); err != nil {
		return err
	}
	info, err := d.root.Lstat(parent)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("root directory %q changed during discovery", parent)
	}
	if !strings.ContainsAny(component, "*?[") {
		if err := d.spend(); err != nil {
			return err
		}
		name := path.Join(parent, component)
		info, err := d.root.Lstat(name)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.IsDir() {
			return visit(name)
		}
		return nil
	}
	dir, err := d.root.Open(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		// Read only a bounded chunk. Even a directory containing millions of files
		// cannot allocate an unbounded listing before the budget is checked.
		entries, readErr := dir.ReadDir(64)
		for _, entry := range entries {
			if err := d.spend(); err != nil {
				return err
			}
			if !entry.IsDir() || reservedCacheRoot(entry.Name(), parent == ".") {
				continue
			}
			matched, err := path.Match(component, entry.Name())
			if err != nil {
				return err
			}
			if matched {
				if err := visit(path.Join(parent, entry.Name())); err != nil {
					return err
				}
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
