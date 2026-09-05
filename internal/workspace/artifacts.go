package workspace

import (
	"fmt"

	"github.com/lydakis/errand/internal/pathpolicy"
)

// Artifacts declares additional retained outputs. Nil inherits the prior
// layer; an explicit empty list clears its declarations.
type Artifacts struct {
	Paths []string `toml:"paths"`
}

func (a *Artifacts) UnmarshalTOML(value any) error {
	table, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("artifacts must be a table")
	}
	*a = Artifacts{}
	for key, raw := range table {
		if key != "paths" {
			return fmt.Errorf("unsupported artifacts setting %q", key)
		}
		values, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("artifacts.paths must be an array of paths")
		}
		a.Paths = make([]string, 0, len(values))
		for _, raw := range values {
			value, ok := raw.(string)
			if !ok {
				return fmt.Errorf("artifacts.paths must contain only strings")
			}
			a.Paths = append(a.Paths, value)
		}
	}
	return pathpolicy.ValidateArtifacts(a.Paths)
}
