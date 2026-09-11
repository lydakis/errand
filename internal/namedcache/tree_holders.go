package namedcache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lydakis/errand/internal/proto"
)

// AcquireTree pins snapshot storage for an independent job. It does
// not install workspace links or infer that this shared path owns processes.
// Commands must follow the cooperative cache access contract.
func (s *Store) AcquireTree(ctx context.Context, key Key, jobID string) (string, error) {
	if err := key.validate(); err != nil {
		return "", err
	}
	if !proto.ValidULID(jobID) {
		return "", fmt.Errorf("invalid tree cache holder")
	}
	if err := s.lock(ctx); err != nil {
		return "", err
	}
	defer s.unlock()
	name := key.hash()
	r, err := s.readRecord(name)
	if os.IsNotExist(err) {
		if _, existsErr := s.root.Lstat(name); !os.IsNotExist(existsErr) {
			return "", fmt.Errorf("incomplete named cache %s: %w", name, err)
		}
		r = record{Version: 2, Entry: Entry{Key: key, Tree: true, LastUsed: s.now(), BytesUnknown: true}}
		if err := s.create(name, r); err != nil {
			return "", err
		}
	} else {
		if err != nil {
			return "", err
		}
		if r.LeaseID != "" {
			return "", ErrBusy
		}
		if err := s.validateData(name); err != nil {
			if !os.IsNotExist(err) {
				return "", err
			}
			// A shared command may delete the root. Repair only after every
			// durable holder is gone, including holders unknown to this daemon.
			if r.Version == 2 {
				holders, readErr := s.readHolders(name)
				if readErr != nil {
					return "", readErr
				}
				if len(holders) != 0 {
					return "", ErrBusy
				}
			}
			if err := s.root.Mkdir(name+"/data", 0700); err != nil {
				return "", err
			}
			if err := s.sync(name); err != nil {
				return "", err
			}
			r.Bytes, r.BytesUnknown = 0, true
		}
		if r.Version == 1 {
			if err := s.root.Mkdir(name+"/holders", 0700); err != nil && !os.IsExist(err) {
				return "", err
			}
			info, err := s.root.Lstat(name + "/holders")
			if err != nil || !info.IsDir() {
				return "", fmt.Errorf("invalid tree cache holder directory")
			}
			holders, err := s.readHolders(name)
			if err != nil {
				return "", err
			}
			if len(holders) != 0 {
				return "", fmt.Errorf("legacy cache has unresolved tree holders")
			}
		}
		if held, err := s.hasHolder(name, jobID); err != nil {
			return "", err
		} else if held {
			return filepath.Join(s.dir, name, "data"), nil
		}
		r.Version, r.Tree, r.LastUsed = 2, true, s.now()
		if err := s.write(name, r); err != nil {
			return "", err
		}
	}
	if err := s.addHolder(name, jobID); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, name, "data"), nil
}

// ReleaseTree removes only this job's pin, after process cleanup. A failure
// must retain the pin for recovery, never discard a sibling's shared data.
// Byte accounting happens during GC, away from the job's completion path.
func (s *Store) ReleaseTree(ctx context.Context, key Key, jobID string) error {
	if err := key.validate(); err != nil {
		return err
	}
	if !proto.ValidULID(jobID) {
		return ErrLeaseMismatch
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	name := key.hash()
	r, err := s.readRecord(name)
	if err != nil {
		return err
	}
	if !r.Tree {
		return ErrLeaseMismatch
	}

	if held, err := s.hasHolder(name, jobID); err != nil {
		return err
	} else if !held {
		return nil
	}
	r.LastUsed = s.now()
	if err := s.write(name, r); err != nil {
		return err
	}
	if err := s.root.Remove(name + "/holders/" + jobID); err != nil {
		return err
	}
	return s.sync(name + "/holders")
}
