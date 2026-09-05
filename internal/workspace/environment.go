package workspace

import (
	"fmt"
	"strings"
)

// Environment contains non-secret literals and names to forward. A nil Pass
// inherits forwarding; a non-nil empty list explicitly clears it.
type Environment struct {
	Set  map[string]string `toml:"set,omitempty"`
	Pass []string          `toml:"pass"`
}

func ValidateEnvironmentName(name string) error {
	if name == "" || strings.ContainsAny(name, "=\x00") {
		return fmt.Errorf("environment name must be non-empty and contain neither '=' nor NUL")
	}
	return nil
}

func (e *Environment) UnmarshalTOML(value any) error {
	table, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("env must be a table")
	}
	*e = Environment{}
	for key, raw := range table {
		switch key {
		case "set":
			values, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("env.set must be a table of strings")
			}
			e.Set = make(map[string]string, len(values))
			for name, raw := range values {
				if err := ValidateEnvironmentName(name); err != nil {
					return err
				}
				v, ok := raw.(string)
				if !ok || strings.ContainsRune(v, 0) {
					return fmt.Errorf("env.set[%q] must be a string without NUL", name)
				}
				e.Set[name] = v
			}
		case "pass":
			values, ok := raw.([]any)
			if !ok {
				return fmt.Errorf("env.pass must be an array of names")
			}
			e.Pass = make([]string, 0, len(values))
			for _, raw := range values {
				name, ok := raw.(string)
				if !ok {
					return fmt.Errorf("env.pass must contain only names")
				}
				if err := ValidateEnvironmentName(name); err != nil {
					return err
				}
				e.Pass = append(e.Pass, name)
			}
		default:
			return fmt.Errorf("unsupported env setting %q", key)
		}
	}
	for _, name := range e.Pass {
		if _, ok := e.Set[name]; ok {
			return fmt.Errorf("env defines %q in both set and pass", name)
		}
	}
	return nil
}
