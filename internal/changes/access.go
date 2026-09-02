package changes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
)

type treeAccess struct {
	root         *os.Root
	rootPath     string
	rootIdentity fsidentity.Identity
	ownsRoot     bool
	original     map[string]fs.FileMode
	physical     map[string]uint32
	identity     map[string]fsidentity.Identity
	size         int64
}

type treePathFilter func(rel string, info fs.FileInfo) (include bool, descend bool)

func makeTreeAccessible(rootPath string) (*treeAccess, error) {
	return makeTreeAccessibleContext(context.Background(), rootPath, -1, -1)
}

func makeTreeAccessibleContext(ctx context.Context, rootPath string, maxEntries int, maxBytes int64) (*treeAccess, error) {
	return makeTreeAccessibleFilteredContext(ctx, rootPath, maxEntries, maxBytes, nil)
}

func makeTreeAccessibleFilteredContext(
	ctx context.Context,
	rootPath string,
	maxEntries int,
	maxBytes int64,
	filter treePathFilter,
) (*treeAccess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rootPath, root, rootIdentity, originalRootMode, physicalRootMode, rootWidened, err := openAccessibleTreeRoot(rootPath)
	if err != nil {
		return nil, err
	}
	access, err := makeTreeAccessibleAtRootFilteredContext(ctx, root, ".", true, maxEntries, maxBytes, filter)
	if err != nil {
		return nil, errors.Join(err, root.Close(), restoreTreeRootMode(rootPath, rootIdentity, originalRootMode, rootWidened))
	}
	if rootWidened {
		access.original["."] = originalRootMode
		access.physical["."] = uint32(physicalRootMode)
		access.identity["."] = rootIdentity
	}
	access.rootPath = rootPath
	access.rootIdentity = rootIdentity
	access.ownsRoot = true
	return access, nil
}

func makeSubtreeAccessibleAtRoot(root *os.Root, rel string) (*treeAccess, error) {
	return makeSubtreeAccessibleAtRootContext(context.Background(), root, rel, -1, -1)
}

func makeSubtreeAccessibleAtRootContext(
	ctx context.Context,
	root *os.Root,
	rel string,
	maxEntries int,
	maxBytes int64,
) (*treeAccess, error) {
	return makeTreeAccessibleAtRootContext(ctx, root, rel, false, maxEntries, maxBytes)
}

func makeTreeAccessibleAtRoot(root *os.Root, start string, excludeReserved bool) (*treeAccess, error) {
	return makeTreeAccessibleAtRootContext(context.Background(), root, start, excludeReserved, -1, -1)
}

func makeTreeAccessibleAtRootContext(
	ctx context.Context,
	root *os.Root,
	start string,
	excludeReserved bool,
	maxEntries int,
	maxBytes int64,
) (*treeAccess, error) {
	return makeTreeAccessibleAtRootFilteredContext(ctx, root, start, excludeReserved, maxEntries, maxBytes, nil)
}

func makeTreeAccessibleAtRootFilteredContext(
	ctx context.Context,
	root *os.Root,
	start string,
	excludeReserved bool,
	maxEntries int,
	maxBytes int64,
	filter treePathFilter,
) (*treeAccess, error) {
	access := &treeAccess{
		root: root, original: make(map[string]fs.FileMode), physical: make(map[string]uint32),
		identity: make(map[string]fsidentity.Identity),
	}
	entries := 0
	var logicalBytes int64
	var visit func(string) error
	visit = func(rel string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if excludeReserved && rel != "." && (pathContainsGitMetadata(rel) || pathUsesApplyTransaction(rel)) {
			return nil
		}
		info, err := root.Lstat(rel)
		if err != nil {
			return err
		}
		include, descend := true, info.IsDir()
		if filter != nil {
			include, descend = filter(rel, info)
		}
		if !include && !descend {
			return nil
		}
		if rel != "." && include {
			if maxEntries >= 0 && entries >= maxEntries {
				return fmt.Errorf("%w: workspace exceeds %d entries", ErrEntryLimitExceeded, maxEntries)
			}
			entries++
			if info.Mode().IsRegular() {
				if maxBytes >= 0 && info.Size() > maxBytes-logicalBytes {
					return fmt.Errorf("%w: workspace exceeds %d bytes", ErrByteLimitExceeded, maxBytes)
				}
				logicalBytes += info.Size()
			}
		}
		identity, err := fsidentity.FromInfo(info)
		if err != nil {
			return err
		}
		if include {
			access.size += info.Size()
		}
		mode := info.Mode().Perm()
		physical := mode
		if rel != "." {
			switch {
			case info.IsDir():
				physical |= 0o700
			case info.Mode().IsRegular():
				physical |= 0o400
			}
			access.original[rel] = mode
			access.physical[rel] = uint32(physical)
			access.identity[rel] = identity
			if physical != mode {
				if err := root.Chmod(rel, physical); err != nil {
					return err
				}
				after, statErr := root.Lstat(rel)
				if statErr != nil {
					return statErr
				}
				afterIdentity, identityErr := fsidentity.FromInfo(after)
				if identityErr != nil || afterIdentity != identity ||
					after.Mode().Type() != info.Mode().Type() || after.Mode().Perm() != physical {
					return fmt.Errorf("workspace path %q changed while preparing change retention", rel)
				}
			}
		}
		if !info.IsDir() || !descend {
			return nil
		}
		dir, err := root.Open(rel)
		if err != nil {
			return err
		}
		opened, statErr := dir.Stat()
		if statErr != nil {
			dir.Close()
			return statErr
		}
		openedIdentity, identityErr := fsidentity.FromInfo(opened)
		if identityErr != nil || openedIdentity != identity || !opened.IsDir() {
			dir.Close()
			return fmt.Errorf("workspace path %q changed while preparing change retention", rel)
		}
		for {
			if err := ctx.Err(); err != nil {
				dir.Close()
				return err
			}
			entries, readErr := dir.ReadDir(256)
			for _, entry := range entries {
				child := entry.Name()
				if rel != "." {
					child = path.Join(rel, child)
				}
				if err := visit(child); err != nil {
					dir.Close()
					return err
				}
			}
			if errors.Is(readErr, io.EOF) {
				return dir.Close()
			}
			if readErr != nil {
				return errors.Join(readErr, dir.Close())
			}
		}
	}
	if err := visit(start); err != nil {
		return nil, errors.Join(err, access.restore())
	}
	return access, nil
}

func makeManifestAccessible(rootPath string, manifest proto.Manifest) (*treeAccess, error) {
	return makeManifestAccessibleContext(context.Background(), rootPath, manifest)
}

func makeManifestAccessibleContext(ctx context.Context, rootPath string, manifest proto.Manifest) (*treeAccess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rootPath, root, rootIdentity, originalRootMode, physicalRootMode, rootWidened, err := openAccessibleTreeRoot(rootPath)
	if err != nil {
		return nil, err
	}
	access := &treeAccess{
		root: root, rootPath: rootPath, rootIdentity: rootIdentity, ownsRoot: true,
		original: make(map[string]fs.FileMode), physical: make(map[string]uint32),
		identity: make(map[string]fsidentity.Identity),
	}
	if rootWidened {
		access.original["."] = originalRootMode
		access.physical["."] = uint32(physicalRootMode)
		access.identity["."] = rootIdentity
	}
	paths := map[string]struct{}{".": {}}
	for _, entry := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, access.restore())
		}
		for current := entry.Path; current != "."; current = path.Dir(current) {
			paths[current] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(paths))
	for rel := range paths {
		ordered = append(ordered, rel)
	}
	sort.Slice(ordered, func(i, j int) bool {
		leftDepth := strings.Count(ordered[i], "/")
		rightDepth := strings.Count(ordered[j], "/")
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return ordered[i] < ordered[j]
	})
	for _, rel := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, access.restore())
		}
		info, err := root.Lstat(rel)
		if err != nil {
			return nil, errors.Join(err, access.restore())
		}
		identity, err := fsidentity.FromInfo(info)
		if err != nil {
			return nil, errors.Join(err, access.restore())
		}
		if rel == "." {
			if !info.IsDir() || identity != rootIdentity {
				return nil, errors.Join(fmt.Errorf("retained tree root changed while it was opened"), access.restore())
			}
			continue
		}
		mode := info.Mode().Perm()
		physical := mode
		switch {
		case info.IsDir():
			physical |= 0o700
		case info.Mode().IsRegular():
			physical |= 0o400
		}
		access.original[rel] = mode
		access.physical[rel] = uint32(physical)
		access.identity[rel] = identity
		if physical != mode {
			if err := root.Chmod(rel, physical); err != nil {
				return nil, errors.Join(err, access.restore())
			}
			after, statErr := root.Lstat(rel)
			if statErr != nil {
				return nil, errors.Join(statErr, access.restore())
			}
			afterIdentity, identityErr := fsidentity.FromInfo(after)
			if identityErr != nil || afterIdentity != identity ||
				after.Mode().Type() != info.Mode().Type() || after.Mode().Perm() != physical {
				return nil, errors.Join(fmt.Errorf("workspace path %q changed while preparing change retention", rel), access.restore())
			}
		}
	}
	return access, nil
}

func openAccessibleTreeRoot(rootPath string) (
	string, *os.Root, fsidentity.Identity, fs.FileMode, fs.FileMode, bool, error,
) {
	abs, err := filepath.Abs(rootPath)
	if err != nil {
		return "", nil, fsidentity.Identity{}, 0, 0, false, err
	}
	rootIdentity, info, err := fsidentity.Lstat(abs)
	if err != nil {
		return "", nil, fsidentity.Identity{}, 0, 0, false, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return "", nil, fsidentity.Identity{}, 0, 0, false, fmt.Errorf("retained tree root is not a directory")
	}
	original := info.Mode().Perm()
	physical := original | 0o700
	widened := physical != original
	if widened {
		if err := os.Chmod(abs, physical); err != nil {
			return "", nil, fsidentity.Identity{}, 0, 0, false, err
		}
		afterIdentity, after, statErr := fsidentity.Lstat(abs)
		if statErr != nil || afterIdentity != rootIdentity || !after.IsDir() || after.Mode().Perm() != physical {
			return "", nil, fsidentity.Identity{}, 0, 0, false, errors.Join(
				fmt.Errorf("retained tree root changed while preparing change retention"), statErr,
				restoreTreeRootMode(abs, rootIdentity, original, true),
			)
		}
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return "", nil, fsidentity.Identity{}, 0, 0, false,
			errors.Join(err, restoreTreeRootMode(abs, rootIdentity, original, widened))
	}
	opened, err := root.Lstat(".")
	if err != nil {
		return "", nil, fsidentity.Identity{}, 0, 0, false,
			errors.Join(err, root.Close(), restoreTreeRootMode(abs, rootIdentity, original, widened))
	}
	openedIdentity, identityErr := fsidentity.FromInfo(opened)
	if identityErr != nil || !opened.IsDir() || openedIdentity != rootIdentity {
		return "", nil, fsidentity.Identity{}, 0, 0, false, errors.Join(
			fmt.Errorf("retained tree root changed while it was opened"), identityErr,
			root.Close(), restoreTreeRootMode(abs, rootIdentity, original, widened),
		)
	}
	return abs, root, rootIdentity, original, physical, widened, nil
}

func restoreTreeRootMode(rootPath string, identity fsidentity.Identity, mode fs.FileMode, widened bool) error {
	if !widened {
		return nil
	}
	currentIdentity, info, err := fsidentity.Lstat(rootPath)
	if err != nil {
		return err
	}
	if currentIdentity != identity || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("retained tree root changed while restoring change retention permissions")
	}
	if info.Mode().Perm() == mode {
		return nil
	}
	return os.Chmod(rootPath, mode)
}

func (a *treeAccess) logicalize(manifest *proto.Manifest) {
	for i := range manifest.Entries {
		if mode, ok := a.original[manifest.Entries[i].Path]; ok {
			manifest.Entries[i].Mode = uint32(mode)
		}
	}
}

func (a *treeAccess) logicalizeRebased(manifest *proto.Manifest, actualRoot, logicalRoot string) {
	for i := range manifest.Entries {
		actual := actualRoot + strings.TrimPrefix(manifest.Entries[i].Path, logicalRoot)
		if mode, ok := a.original[actual]; ok {
			manifest.Entries[i].Mode = uint32(mode)
		}
	}
}

func (a *treeAccess) closeWithoutRestore() error {
	if a.ownsRoot {
		return a.root.Close()
	}
	return nil
}

func (a *treeAccess) restore() error {
	paths := make([]string, 0, len(a.original))
	for rel := range a.original {
		paths = append(paths, rel)
	}
	sort.Slice(paths, func(i, j int) bool {
		leftDepth := strings.Count(paths[i], "/")
		rightDepth := strings.Count(paths[j], "/")
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		return paths[i] > paths[j]
	})
	var restoreErr error
	for _, rel := range paths {
		info, err := a.root.Lstat(rel)
		if err != nil {
			restoreErr = errors.Join(restoreErr, err)
			continue
		}
		identity, identityErr := fsidentity.FromInfo(info)
		if identityErr != nil || identity != a.identity[rel] {
			restoreErr = errors.Join(restoreErr,
				fmt.Errorf("workspace path %q changed while restoring change retention permissions", rel))
			continue
		}
		if info.Mode()&fs.ModeSymlink != 0 || info.Mode().Perm() == a.original[rel] {
			continue
		}
		restoreErr = errors.Join(restoreErr, a.root.Chmod(rel, a.original[rel]))
	}
	if a.ownsRoot {
		restoreErr = errors.Join(restoreErr, a.root.Close())
	}
	return restoreErr
}

// TreeSize measures a retained tree while preserving its logical permissions.
func TreeSize(rootPath string) (int64, error) {
	info, err := os.Lstat(rootPath)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return info.Size(), nil
	}
	access, err := makeTreeAccessible(rootPath)
	if err != nil {
		return 0, err
	}
	return access.size, access.restore()
}

// RemoveTree removes a retained tree after granting temporary owner traversal.
func RemoveTree(rootPath string) error {
	info, err := os.Lstat(rootPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return os.Remove(rootPath)
	}
	access, err := makeTreeAccessible(rootPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	identity, info, identityErr := fsidentity.Lstat(rootPath)
	if identityErr != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 ||
		identity != access.rootIdentity {
		return errors.Join(
			fmt.Errorf("retained tree root changed before removal"),
			identityErr, access.restore(),
		)
	}
	removeErr := os.RemoveAll(rootPath)
	if removeErr != nil {
		return errors.Join(removeErr, access.restore())
	}
	return access.closeWithoutRestore()
}

func removeTreeAtRoot(root *os.Root, rel string) error {
	access, err := makeSubtreeAccessibleAtRoot(root, rel)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	removeErr := root.RemoveAll(rel)
	if removeErr != nil {
		return errors.Join(removeErr, access.restore())
	}
	return access.closeWithoutRestore()
}
