package namedcache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/lydakis/errand/internal/proto"
)

// Dry-run budget accounting excludes exactly the generations it would collect.
// Walking stays outside the metadata lock; GC rechecks LastUsed and holders
// before adopting the measurement, as it does for the other tree scans.
func (s *Store) measureRetainedTrees(ctx context.Context, name string) (int64, error) {
	root, err := s.root.OpenRoot(name)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	current, err := currentTree(root)
	if err != nil {
		return 0, err
	}
	entries, err := fs.ReadDir(root.FS(), "trees")
	if err != nil {
		return 0, err
	}
	var size int64
	for _, entry := range entries {
		child := entry.Name()
		if "trees/"+child != current && strings.HasPrefix(child, "generation-") && proto.ValidULID(strings.TrimPrefix(child, "generation-")) {
			continue
		}
		bytes, err := s.measureConcurrent(ctx, name+"/trees/"+child)
		if err != nil {
			return 0, err
		}
		if bytes > math.MaxInt64-size {
			return 0, fmt.Errorf("named cache byte count overflow")
		}
		size += bytes
	}
	return size, nil
}

// Called by GC only. A locked idle check excludes both publication and new
// acquisitions while obsolete generations are detached; deletion stays outside
// the metadata lock. A crash leaves an ordinary GC tombstone.
func (s *Store) collectUnusedTrees(ctx context.Context, key Key, dryRun bool) (int, error) {
	if err := s.lock(ctx); err != nil {
		return 0, err
	}
	var tombs []string
	count := 0
	err := func() error {
		defer s.unlock()
		r, err := s.read(key.hash())
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if r.Protected() || !r.Tree {
			return nil
		}
		root, err := s.root.OpenRoot(key.hash())
		if err != nil {
			return err
		}
		defer root.Close()
		current, err := currentTree(root)
		if err != nil {
			return err
		}
		entries, err := fs.ReadDir(root.FS(), "trees")
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if "trees/"+name == current || !strings.HasPrefix(name, "generation-") || !proto.ValidULID(strings.TrimPrefix(name, "generation-")) {
				continue
			}
			count++
			if dryRun {
				continue
			}
			tomb := ".gc-" + key.hash() + "-" + proto.NewULID()
			if err := s.root.Rename(key.hash()+"/trees/"+name, tomb); err != nil {
				return err
			}
			s.markRetiring(tomb)
			tombs = append(tombs, tomb)
		}
		if len(tombs) > 0 {
			return errors.Join(s.sync(key.hash()+"/trees"), s.sync("."))
		}
		return nil
	}()
	for _, tomb := range tombs {
		err = errors.Join(err, s.removeRetired(tomb))
	}
	return count, err
}

func restoreStage(key Key, holder, path string) string {
	return filepath.Join(filepath.Dir(path), ".errand-cache-tree-"+key.hash()+"-"+holder)
}

// A holder's stable staging name lets recovery remove exactly the directory
// created for that job, without sweeping arbitrary user-created prefixes.
func (s *Store) DiscardRestore(ctx context.Context, key Key, holder, workspace, path string) error {
	if err := key.validate(); err != nil {
		return err
	}
	if !proto.ValidULID(holder) {
		return fmt.Errorf("invalid restore holder")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := treeWorkspace(workspace, path, false)
	// A missing or non-directory parent cannot contain this job's stage.
	// Symlink parents remain errors; never follow them to clean another tree.
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	return removeTree(filepath.Join(workspace, restoreStage(key, holder, path)))
}

// Only directories need writable permissions for unlinking. Never chmod
// regular files here: a retired tree can share their inodes with running jobs.
func removeTree(path string) error {
	if err := os.RemoveAll(path); err == nil {
		return nil
	}
	err := filepath.WalkDir(path, func(path string, entry fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			return os.Chmod(path, info.Mode().Perm()|0700)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}
