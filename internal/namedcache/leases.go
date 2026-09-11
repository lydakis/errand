package namedcache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lydakis/errand/internal/proto"
)

// LeaseState reports only the requested job's rights. Lookup never scans
// unrelated cache records or sibling holder files.
type LeaseState struct{ Directory, Tree bool }

func (s *Store) LookupLease(ctx context.Context, key Key, jobID string) (LeaseState, error) {
	if err := key.validate(); err != nil {
		return LeaseState{}, err
	}
	if !proto.ValidULID(jobID) {
		return LeaseState{}, fmt.Errorf("invalid job ID")
	}
	if err := s.lock(ctx); err != nil {
		return LeaseState{}, err
	}
	defer s.unlock()
	return s.lookupLease(key, jobID)
}

func (s *Store) lookupLease(key Key, jobID string) (LeaseState, error) {
	name := key.hash()
	r, err := s.readRecord(name)
	if os.IsNotExist(err) {
		if _, statErr := s.root.Lstat(name); os.IsNotExist(statErr) {
			return LeaseState{}, nil
		}
	}
	if err != nil {
		return LeaseState{}, err
	}
	state := LeaseState{Directory: r.LeaseID == jobID}
	if r.Version == 2 {
		held, err := s.hasHolder(name, jobID)
		if err != nil {
			return LeaseState{}, err
		}
		if held && r.LeaseID != "" {
			return LeaseState{}, fmt.Errorf("conflicting cache leases")
		}
		state.Tree = held
	}
	return state, nil
}

// LeasePaths returns only currently exclusive directory leases from the job's
// durable selection receipt. Shared tree paths never establish process ownership.
func (s *Store) LeasePaths(ctx context.Context, jobID string, keys []Key) ([]string, error) {
	if !proto.ValidULID(jobID) {
		return nil, fmt.Errorf("invalid job ID")
	}
	if len(keys) == 0 {
		return nil, nil
	}
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer s.unlock()
	root, err := filepath.EvalSymlinks(s.dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := key.validate(); err != nil {
			return nil, err
		}
		state, err := s.lookupLease(key, jobID)
		if err != nil {
			return nil, err
		}
		if state.Directory {
			paths = append(paths, filepath.Join(root, key.hash(), "data"))
		}
	}
	return paths, nil
}
