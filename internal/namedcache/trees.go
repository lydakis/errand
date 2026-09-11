package namedcache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/lydakis/errand/internal/proto"
)

// Cached trees have workspace-local directories and links. Only regular
// files may share inodes. A short-lived per-key gate protects materialization
// and replacement, never command execution or unrelated caches.
type treeGate struct {
	token chan struct{}
	users int
}

func (s *Store) lockTree(ctx context.Context, key Key, holder string) (string, func(), error) {
	s.operations.RLock()
	state, err := s.LookupLease(ctx, key, holder)
	if err != nil || !state.Tree {
		s.operations.RUnlock()
		return "", nil, errors.Join(err, ErrLeaseMismatch)
	}
	name := key.hash()
	s.treeGateMu.Lock()
	if s.treeGates == nil {
		s.treeGates = make(map[string]*treeGate)
	}
	g := s.treeGates[name]
	if g == nil {
		g = &treeGate{token: make(chan struct{}, 1)}
		s.treeGates[name] = g
	}
	g.users++
	s.treeGateMu.Unlock()
	release := func() {
		s.treeGateMu.Lock()
		g.users--
		if g.users == 0 {
			delete(s.treeGates, name)
		}
		s.treeGateMu.Unlock()
		s.operations.RUnlock()
	}
	select {
	case g.token <- struct{}{}:
		return filepath.Join(s.dir, name), func() { <-g.token; release() }, nil
	case <-ctx.Done():
		release()
		return "", nil, ctx.Err()
	}
}

// RestoreTree creates a real directory, publishing it only after the
// complete layout is ready. The returned fingerprint detects changed writers;
// an unchanged test job must not overwrite a newer installation.
func (s *Store) RestoreTree(ctx context.Context, key Key, holder, workspace, path string, checkpoint ...func(string) error) (string, error) {
	return s.restoreTree(ctx, key, holder, workspace, path, !preferTreeClone, checkpoint...)
}

func (s *Store) restoreTree(ctx context.Context, key Key, holder, workspace, path string, linked bool, checkpoint ...func(string) error) (string, error) {
	data, unlock, err := s.lockTree(ctx, key, holder)
	if err != nil {
		return "", err
	}
	defer unlock()
	root, err := treeWorkspace(workspace, path, true)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if _, err := root.Lstat(path); !os.IsNotExist(err) {
		return "", fmt.Errorf("cache destination %q already exists or is inaccessible", path)
	}
	cache, err := os.OpenRoot(data)
	if err != nil {
		return "", err
	}
	defer cache.Close()
	stage := restoreStage(key, holder, path)
	if err := root.Mkdir(stage, 0700); err != nil {
		return "", err
	}
	defer removeTree(filepath.Join(workspace, stage))
	generation, err := currentTree(cache)
	if err != nil {
		return "", err
	}
	source := filepath.Join(data, "data")
	if generation != "" {
		source = filepath.Join(data, generation, "tree")
	}
	var fingerprint string
	if err := copyTree(ctx, source, filepath.Join(workspace, stage), linked, &fingerprint); err != nil {
		if !errors.Is(err, errTreeLink) {
			return "", err
		}
		// One mode per tree makes its change-detection contract stable even
		// when links disappear later. Cross-filesystem restores use private files.
		if err := removeTree(filepath.Join(workspace, stage)); err != nil {
			return "", err
		}
		if err := root.Mkdir(stage, 0700); err != nil {
			return "", err
		}
		if err := copyTree(ctx, source, filepath.Join(workspace, stage), false, &fingerprint); err != nil {
			return "", err
		}
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Persist the comparison mode before exposing the restored directory.
	// Recovery can retry a missing directory, but must never guess whether an
	// already exposed file is shared after a crash between rename and receipt.
	for _, save := range checkpoint {
		if err := save(fingerprint); err != nil {
			return "", err
		}
	}
	if err := root.Rename(stage, path); err != nil {
		return "", err
	}
	if err := syncTreeDirectory(filepath.Join(workspace, filepath.Dir(path))); err != nil {
		return "", err
	}
	return fingerprint, nil
}

// PublishTree saves changes after the command's process scope has stopped.
// Concurrent changed workspaces use last-completed-publication wins, like
// replacing an installed tree locally. Read-only jobs never publish.
func (s *Store) PublishTree(ctx context.Context, key Key, holder, workspace, path, baseline string, consume bool) (fingerprint string, retErr error) {
	now, err := TreeFingerprint(ctx, workspace, path, baseline)
	if err != nil {
		return "", err
	}
	if now == baseline {
		return now, nil
	}
	data, unlock, err := s.lockTree(ctx, key, holder)
	if err != nil {
		return "", err
	}
	defer unlock()
	cache, err := os.OpenRoot(data)
	if err != nil {
		return "", err
	}
	defer cache.Close()
	old, err := currentTree(cache)
	if err != nil {
		return "", err
	}
	if err := cache.Mkdir("trees", 0700); err != nil && !os.IsExist(err) {
		return "", err
	}
	generation := "trees/generation-" + proto.NewULID()
	if err := cache.Mkdir(generation, 0700); err != nil {
		return "", err
	}
	keep := false
	moved := false
	defer func() {
		if !keep {
			if moved {
				if err := os.Rename(filepath.Join(data, generation, "tree"), filepath.Join(workspace, path)); err != nil {
					retErr = errors.Join(retErr, fmt.Errorf("restoring unpublished cache tree: %w", err))
					return // retain the only remaining copy for recovery/GC
				}
			}
			_ = cache.RemoveAll(generation)
		}
	}()
	if now != "absent" && consume {
		// Go's os.Rename requires a nonexistent directory destination.
		moved = os.Rename(filepath.Join(workspace, path), filepath.Join(data, generation, "tree")) == nil
	}
	if !moved {
		if err := cache.Mkdir(generation+"/tree", 0700); err != nil {
			return "", err
		}
	}
	if now != "absent" {
		// Once an ephemeral command is settled, its directory can become the
		// saved tree directly. No second traversal or copy of package bytes.
		if !moved {
			source, target := filepath.Join(workspace, path), filepath.Join(data, generation, "tree")
			if err := copyTree(ctx, source, target, strings.HasPrefix(baseline, "links:"), nil); err != nil {
				if !errors.Is(err, errTreeLink) {
					return "", err
				}
				if err := removeTree(target); err != nil {
					return "", err
				}
				if err := cache.Mkdir(generation+"/tree", 0700); err != nil {
					return "", err
				}
				if err := copyTree(ctx, source, target, false, nil); err != nil {
					return "", err
				}
				// Only the saved copy is private. Keep the workspace's comparison
				// mode: its existing files may still share inodes with siblings.
			}
		}
	}
	if err := syncTreeDirectory(filepath.Join(data, generation)); err != nil {
		return "", err
	}

	if err := syncTreeDirectory(filepath.Join(data, "trees")); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	tmp := ".current-" + proto.NewULID()
	if err := cache.Symlink(generation, tmp); err != nil {
		return "", err
	}
	defer cache.Remove(tmp)
	if err := cache.Rename(tmp, "tree-current"); err != nil {
		return "", err
	}
	keep = true
	if err := syncTreeDirectory(data); err != nil {
		return now, err
	}
	// Materialized files have their own inode references. Old generations
	// need no pin beyond materialization; symlink targets are never rewritten.
	if old != "" {
		if err := s.retireTree(ctx, key.hash(), old); err != nil {
			return now, err
		}
	} else {
		// Retire the payload inherited from a v1 directory cache only after
		// its first saved tree has been published durably.
		entries, err := fs.ReadDir(cache.FS(), "data")
		if err != nil {
			return now, err
		}
		for _, entry := range entries {
			if err := cache.RemoveAll("data/" + entry.Name()); err != nil {
				if err := s.remove(key.hash() + "/data/" + entry.Name()); err != nil {
					return now, err
				}
			}
		}
	}
	return now, nil
}

// Detach the superseded tree under the metadata lock, then delete it without
// making command completion or the next restore wait for recursive unlinking.
// Close drains these tasks; failed deletion leaves a tomb for ordinary GC.
func (s *Store) retireTree(ctx context.Context, name, generation string) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	tomb := ".gc-" + name + "-" + proto.NewULID()
	if err := s.root.Rename(name+"/"+generation, tomb); err != nil {
		return err
	}
	// Sync both parents of the cross-directory rename before relying on the
	// tombstone for interrupted-retirement recovery.
	if err := errors.Join(s.sync(name+"/trees"), s.sync(".")); err != nil {
		return err
	}
	s.markRetiring(tomb)
	s.retireWG.Go(func() {
		// Most trees are writable. Only traverse permissions if needed.
		err := s.root.RemoveAll(tomb)
		if err != nil {
			_ = s.remove(tomb)
		}
		<-s.mu
		delete(s.retiring, tomb)
		s.unlock()
	})
	return nil
}

func currentTree(root *os.Root) (string, error) {
	name, err := root.Readlink("tree-current")
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(name, "trees/generation-") || !proto.ValidULID(strings.TrimPrefix(name, "trees/generation-")) {
		return "", fmt.Errorf("invalid installed cache generation")
	}
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("installed cache generation is not a directory")
	}
	tree, err := root.Lstat(name + "/tree")
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !tree.IsDir() {
		return "", fmt.Errorf("installed cache payload is not a directory")
	}
	return name, nil
}

func treeWorkspace(workspace, path string, createParents bool) (*os.Root, error) {
	if !filepath.IsLocal(path) || path == "." {
		return nil, fmt.Errorf("invalid installed cache path %q", path)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(path)
	part := ""
	if parent != "." {
		for _, name := range strings.Split(parent, string(filepath.Separator)) {
			part = filepath.Join(part, name)
			if createParents {
				if err := root.Mkdir(part, 0700); err != nil && !os.IsExist(err) {
					root.Close()
					return nil, err
				} else if err == nil {
					if err := syncTreeDirectory(filepath.Join(workspace, filepath.Dir(part))); err != nil {
						root.Close()
						return nil, err
					}
				}
			}
			info, err := root.Lstat(part)
			if os.IsNotExist(err) && !createParents {
				return root, nil
			}
			if err == nil && !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				root.Close()
				return nil, fmt.Errorf("cache parent %q must be a directory: %w", part, syscall.ENOTDIR)
			}
			if err != nil || !info.IsDir() {
				root.Close()
				return nil, fmt.Errorf("cache parent %q must be a directory", part)
			}
		}
	}
	return root, nil
}

func syncTreeDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
