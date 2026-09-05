package namedcache

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/lydakis/errand/internal/proto"
)

// LeasePaths returns canonical data directories currently leased by jobID.
// Deriving these from durable leases keeps recovery from tracking a cache that
// an old job released and another job has since acquired. The data directory
// need not still exist for its path to identify a process's working directory.
func (s *Store) LeasePaths(ctx context.Context, jobID string) ([]string, error) {
	if !proto.ValidULID(jobID) {
		return nil, fmt.Errorf("invalid job ID")
	}
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer s.unlock()
	entries, err := s.inventory(ctx)
	if err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(s.dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		if entry.LeaseID == jobID {
			paths = append(paths, filepath.Join(root, entry.Key.hash(), "data"))
		}
	}
	return paths, nil
}
