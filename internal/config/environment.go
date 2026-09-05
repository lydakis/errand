package config

import (
	"os"
	"sort"

	"github.com/lydakis/errand/internal/workspace"
)

// EnvironmentVariable is safe to include in JSON diagnostics. Literal values
// stay private, and the resolver does not retain forwarded values.
type EnvironmentVariable struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // literal or passenv
	Source    string `json:"source"`
	Available bool   `json:"available"`
	value     string
}

type environmentLayer struct {
	settings workspace.Environment
	source   string
}

func resolveEnvironment(layers ...environmentLayer) []EnvironmentVariable {
	byName := map[string]EnvironmentVariable{}
	for _, layer := range layers {
		if layer.settings.Pass != nil {
			for name, entry := range byName {
				if entry.Kind == "passenv" {
					delete(byName, name)
				}
			}
			for _, name := range layer.settings.Pass {
				_, available := os.LookupEnv(name)
				byName[name] = EnvironmentVariable{Name: name, Kind: "passenv", Source: layer.source + ".pass", Available: available}
			}
		}
		for name, value := range layer.settings.Set {
			byName[name] = EnvironmentVariable{Name: name, Kind: "literal", Source: layer.source + ".set", Available: true, value: value}
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	var result []EnvironmentVariable
	for _, name := range names {
		result = append(result, byName[name])
	}
	return result
}

func (r EffectiveRun) JobEnvironment() (map[string]string, []string) {
	literals := map[string]string{}
	var pass []string
	for _, entry := range r.Environment {
		if entry.Kind == "literal" {
			literals[entry.Name] = entry.value
		} else {
			pass = append(pass, entry.Name)
		}
	}
	return literals, pass
}

func (r EffectiveRun) MissingEnvironment() []string {
	var missing []string
	for _, entry := range r.Environment {
		if !entry.Available {
			missing = append(missing, entry.Name)
		}
	}
	return missing
}
