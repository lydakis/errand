package config

import (
	"fmt"
	"slices"

	"github.com/lydakis/errand/internal/pathpolicy"
)

func resolveArtifacts(personal, selected, profile, cli []string, personalSource, workspaceSource, profileSource string) ([]string, string, error) {
	paths, source := []string{}, "default: no artifacts"
	for _, layer := range []struct {
		paths  []string
		source string
	}{
		{personal, personalSource + " artifacts.paths"},
		{selected, workspaceSource + " artifacts.paths"},
		{profile, profileSource + " artifacts.paths"},
		{cli, "cli: --artifact/--no-artifacts"},
	} {
		if layer.paths != nil {
			paths, source = slices.Clone(layer.paths), layer.source
		}
	}
	if err := pathpolicy.ValidateArtifacts(paths); err != nil {
		return nil, source, fmt.Errorf("%s: %w", source, err)
	}
	return paths, source, nil
}
