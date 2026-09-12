package config

import (
	"fmt"
	"slices"

	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/workspace"
)

func resolveCaches(root string, personal, selected, profile workspace.Caches, cli []proto.CacheBinding, personalSource, workspaceSource, profileSource string) ([]proto.CacheBinding, map[string]string, string, error) {
	source := "default: no named caches"
	var declarations workspace.Caches
	for _, layer := range []struct {
		caches workspace.Caches
		source string
	}{
		{personal, personalSource}, {selected, workspaceSource}, {profile, profileSource},
	} {
		if layer.caches != nil {
			declarations, source = layer.caches, layer.source+" caches"
		}
	}
	if cli != nil {
		source = "cli: --cache/--no-caches"
		if err := pathpolicy.ValidateCaches(cli); err != nil {
			return nil, nil, source, fmt.Errorf("%s: %w", source, err)
		}
		origins := make(map[string]string, len(cli))
		for _, binding := range cli {
			origins[binding.Name] = source
		}
		return slices.Clone(cli), origins, source, nil
	}
	bindings, origins, err := declarations.Resolve(root)
	if err != nil {
		return nil, nil, source, fmt.Errorf("%s: %w", source, err)
	}
	if declarations == nil {
		bindings = []proto.CacheBinding{}
	}
	for name, origin := range origins {
		origins[name] = source + " (" + origin + ")"
	}
	return bindings, origins, source, nil
}
