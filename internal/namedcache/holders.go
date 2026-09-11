package namedcache

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"syscall"

	"github.com/lydakis/errand/internal/proto"
)

// Each job has a durable empty file. Keeping holders outside record.json
// avoids imposing a new concurrency limit through the metadata size bound.
func (s *Store) readHolders(name string) ([]string, error) {
	dir, err := s.root.OpenFile(name+"/holders", os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	var holders []string
	for {
		entries, err := dir.ReadDir(256)
		for _, entry := range entries {
			if !proto.ValidULID(entry.Name()) {
				return nil, fmt.Errorf("invalid tree cache holder name")
			}
			if held, err := s.hasHolder(name, entry.Name()); err != nil {
				return nil, err
			} else if !held {
				return nil, fmt.Errorf("tree cache holder disappeared during metadata read")
			}
			holders = append(holders, entry.Name())
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	slices.Sort(holders)
	return holders, nil
}

func (s *Store) hasHolder(name, id string) (bool, error) {
	info, err := s.root.Lstat(name + "/holders/" + id)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != 0 {
		return false, fmt.Errorf("invalid tree cache holder")
	}
	return true, nil
}

func (s *Store) addHolder(name, id string) error {
	f, err := s.root.OpenFile(name+"/holders/"+id, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	return s.sync(name + "/holders")
}
