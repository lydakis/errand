package workspace

import (
	"fmt"
	"path"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
)

// Caches maps names to typed declarations. Nil inherits; an explicitly empty
// table clears the prior layer.
type Caches map[string]CacheDeclaration

// CacheDeclaration binds Path exactly when Roots is nil. Otherwise Roots
// selects existing source directories and Path is appended to each one.
type CacheDeclaration struct {
	Roots []string
	Path  string
}

// MarshalTOML keeps exact-path shorthand intact when personal config is saved.
// Delegate value escaping to the same codec used to read configuration.
func (c CacheDeclaration) MarshalTOML() ([]byte, error) {
	p, err := toml.Marshal(c.Path)
	if err != nil {
		return nil, err
	}
	if c.Roots == nil {
		return p, nil
	}
	roots, err := toml.Marshal(c.Roots)
	if err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "{ roots = %s, path = %s }", roots, p), nil
}

func (c *Caches) UnmarshalTOML(value any) error {
	table, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("caches must be a table of named paths or directory groups")
	}
	*c = Caches{}
	var exact []proto.CacheBinding
	for name, raw := range table {
		entry, err := decodeCacheDeclaration(raw)
		if err != nil {
			return fmt.Errorf("cache %q: %w", name, err)
		}
		binding := proto.CacheBinding{Name: name, Path: entry.Path}
		if err := pathpolicy.ValidateCaches([]proto.CacheBinding{binding}); err != nil {
			return fmt.Errorf("cache %q: %w", name, err)
		}
		(*c)[name] = entry
		if entry.Roots == nil {
			exact = append(exact, binding)
		}
	}
	return pathpolicy.ValidateCaches(exact)
}

func decodeCacheDeclaration(raw any) (CacheDeclaration, error) {
	if p, ok := raw.(string); ok {
		return CacheDeclaration{Path: p}, nil
	}
	var group CacheDeclaration
	table, ok := raw.(map[string]any)
	if !ok {
		return group, fmt.Errorf("must be a path string or a table with roots and path")
	}
	for key, raw := range table {
		switch key {
		case "path":
			var ok bool
			group.Path, ok = raw.(string)
			if !ok {
				return group, fmt.Errorf("path must be a string")
			}
		case "roots":
			values, ok := raw.([]any)
			if !ok {
				return group, fmt.Errorf("roots must be an array of directory patterns")
			}
			for _, raw := range values {
				value, ok := raw.(string)
				if !ok {
					return group, fmt.Errorf("roots must contain only strings")
				}
				if err := validateCacheRoot(value); err != nil {
					return group, err
				}
				group.Roots = append(group.Roots, value)
			}
		default:
			return group, fmt.Errorf("unsupported group setting %q", key)
		}
	}
	if group.Path == "" {
		return group, fmt.Errorf("group requires a non-empty path")
	}
	if len(group.Roots) == 0 || len(group.Roots) > 64 {
		return group, fmt.Errorf("roots must contain between 1 and 64 directory patterns")
	}
	return group, nil
}

func validateCacheRoot(pattern string) error {
	if pattern == "" || len(pattern) > pathpolicy.MaxPatternBytes || path.IsAbs(pattern) || path.Clean(pattern) != pattern || pattern == ".." || strings.HasPrefix(pattern, "../") || strings.ContainsAny(pattern, "\\\x00\r\n") {
		return fmt.Errorf("root %q must be a workspace-relative directory pattern", pattern)
	}
	for i, component := range strings.Split(pattern, "/") {
		if strings.Contains(component, "**") {
			return fmt.Errorf("root %q: recursive ** is not supported", pattern)
		}
		if _, err := path.Match(component, ""); err != nil {
			return fmt.Errorf("invalid root pattern %q: %w", pattern, err)
		}
		if reservedCacheRoot(component, i == 0) {
			return fmt.Errorf("root %q uses reserved metadata", pattern)
		}
	}
	return nil
}

func reservedCacheRoot(name string, topLevel bool) bool {
	return strings.EqualFold(name, ".git") || (topLevel && strings.HasPrefix(name, ".errand-change-"))
}
