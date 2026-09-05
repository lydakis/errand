package config

import (
	"fmt"
	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
	"slices"
)

func resolveCaches(personal, selected, profile, cli []proto.CacheBinding, personalSource, workspaceSource, profileSource string) ([]proto.CacheBinding, string, error) {
	caches, source := []proto.CacheBinding{}, "default: no named caches"
	for _, layer := range []struct {
		caches []proto.CacheBinding
		source string
	}{
		{personal, personalSource + " caches"}, {selected, workspaceSource + " caches"}, {profile, profileSource + " caches"}, {cli, "cli: --cache/--no-caches"},
	} {
		if layer.caches != nil {
			caches, source = slices.Clone(layer.caches), layer.source
		}
	}
	if err := pathpolicy.ValidateCaches(caches); err != nil {
		return nil, source, fmt.Errorf("%s: %w", source, err)
	}
	return caches, source, nil
}
