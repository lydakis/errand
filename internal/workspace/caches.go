package workspace

import (
	"fmt"
	"sort"

	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
)

// Caches maps stable cache names to workspace-relative directories.
// An absent table inherits; an explicitly empty table clears the prior layer.
type Caches map[string]string

func (c *Caches) UnmarshalTOML(value any) error {
	table, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("caches must be a table of NAME = PATH entries")
	}
	*c = Caches{}
	for name, raw := range table {
		path, ok := raw.(string)
		if !ok {
			return fmt.Errorf("cache %q path must be a string", name)
		}
		(*c)[name] = path
	}
	return pathpolicy.ValidateCaches(c.Bindings())
}

// Bindings keeps wire ordering deterministic while preserving absent versus
// explicitly empty config tables for inheritance and serialization.
func (c Caches) Bindings() []proto.CacheBinding {
	if c == nil {
		return nil
	}
	bindings := make([]proto.CacheBinding, 0, len(c))
	for name, path := range c {
		bindings = append(bindings, proto.CacheBinding{Name: name, Path: path})
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Name < bindings[j].Name })
	return bindings
}
