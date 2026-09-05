package changes

import (
	"io/fs"
	"path"
	"strings"

	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
)

type retentionSelector struct {
	artifacts map[string]bool
	baseline  map[string]struct{}
	ancestors map[string]struct{}
	matcher   *pathpolicy.Matcher
}

func newRetentionSelector(baseline proto.Manifest, policy proto.SelectionPolicy) (*retentionSelector, error) {
	matcher, err := pathpolicy.Compile(policy)
	if err != nil {
		return nil, err
	}
	selector := &retentionSelector{
		artifacts: make(map[string]bool, len(policy.Artifacts)),
		baseline:  make(map[string]struct{}, len(baseline.Entries)),
		ancestors: make(map[string]struct{}),
		matcher:   matcher,
	}
	for _, name := range policy.Artifacts {
		selector.artifacts[name] = true
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			selector.ancestors[parent] = struct{}{}
		}
	}
	for _, entry := range baseline.Entries {
		selector.baseline[entry.Path] = struct{}{}
		for parent := path.Dir(entry.Path); parent != "."; parent = path.Dir(parent) {
			selector.ancestors[parent] = struct{}{}
		}
	}
	return selector, nil
}

// selectPath returns whether a path participates in the final manifest and
// whether a directory must be traversed. Baseline paths are always selected.
// Declared artifact subtrees override ignores. Ignored ancestors are traversed
// only when needed to reach a baseline path or an artifact declaration.
func (s *retentionSelector) selectPath(rel string, info fs.FileInfo) (bool, bool) {
	if rel == "." {
		return true, true
	}
	if pathContainsGitMetadata(rel) || pathUsesApplyTransaction(rel) {
		return false, false
	}
	if _, ok := s.baseline[rel]; ok {
		return true, info.IsDir()
	}
	for current := rel; current != "."; current = path.Dir(current) {
		if s.artifacts[current] {
			return true, info.IsDir()
		}
	}
	_, requiredAncestor := s.ancestors[rel]
	if s.matcher.Ignored(strings.TrimPrefix(rel, "./"), info.IsDir()) {
		return false, info.IsDir() && requiredAncestor
	}
	return true, info.IsDir()
}
