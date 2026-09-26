package client

import (
	"sort"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/termui"
)

// PathChange is one changed path and what happened to it: A added,
// M modified, D deleted.
type PathChange struct {
	Path string
	Kind byte
}

// Letter renders the change kind the way git status does.
func (c PathChange) Letter(s *termui.Stream) string {
	switch c.Kind {
	case 'A':
		return s.Paint("A", termui.Green)
	case 'D':
		return s.Paint("D", termui.Red)
	default:
		return s.Paint("M", termui.Yellow)
	}
}

// pathChanges classifies a bundle's paths against its base and remote
// manifests. selected limits the result when non-nil.
func pathChanges(bundle proto.ChangeBundle, selected map[string]bool) []PathChange {
	var out []PathChange
	for _, p := range bundle.Paths {
		if selected != nil && !selected[p] {
			continue
		}
		inBase := manifestContainsPath(bundle.BaseManifest, p)
		inRemote := manifestContainsPath(bundle.RemoteManifest, p)
		kind := byte('M')
		switch {
		case !inBase && inRemote:
			kind = 'A'
		case inBase && !inRemote:
			kind = 'D'
		}
		out = append(out, PathChange{Path: p, Kind: kind})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// StagedChanges reads a downloaded change set and classifies its paths.
func StagedChanges(dir string) ([]PathChange, error) {
	if dir == "" {
		return nil, nil
	}
	bundle, err := loadStagedBundle(dir)
	if err != nil {
		return nil, err
	}
	return pathChanges(bundle, nil), nil
}
