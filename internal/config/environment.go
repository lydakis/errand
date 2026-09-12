package config

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/lydakis/errand/internal/workspace"
)

// EnvironmentVariable is safe to include in JSON diagnostics. Literal values
// stay private, and the resolver does not retain forwarded values.
type EnvironmentVariable struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // literal, file, or passenv
	Source    string `json:"source"`
	Available bool   `json:"available"`
	value     string
}

type environmentLayer struct {
	settings  workspace.Environment
	source    string
	directory string
}

func resolveEnvironment(layers ...environmentLayer) ([]EnvironmentVariable, error) {
	// Select replacement lists before folding values. A superseded pass list
	// must not overwrite file or literal defaults that should remain available.
	fileLayer, passLayer := -1, -1
	for i, layer := range layers {
		if layer.settings.Files != nil {
			fileLayer = i
		}
		if layer.settings.Pass != nil {
			passLayer = i
		}
	}
	byName := map[string]EnvironmentVariable{}
	for i, layer := range layers {
		if i == fileLayer {
			for _, name := range layer.settings.Files {
				path := name
				if !filepath.IsAbs(path) {
					path = filepath.Join(layer.directory, path)
				}
				values, err := readEnvironmentFile(path)
				if err != nil {
					return nil, err
				}
				for name, value := range values {
					byName[name] = EnvironmentVariable{Name: name, Kind: "file", Source: layer.source + ".files: " + path, Available: true, value: value}
				}
			}
		}
		if i == passLayer {
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
	return result, nil
}

// JobEnvironment returns values loaded by PrepareExecution.
func (r EffectiveRun) JobEnvironment() (map[string]string, []string) {
	literals := map[string]string{}
	var pass []string
	for _, entry := range r.Environment {
		if entry.Kind != "passenv" {
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
