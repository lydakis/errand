// Package changes owns workspace change collection, verified extraction, and
// conflict-safe application.
package changes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

type Baseline struct {
	Path    string `json:"path"`
	Digest  string `json:"digest,omitempty"`
	Missing bool   `json:"missing,omitempty"`
}

type ApplyResult struct {
	Applied     []string
	States      map[string]string
	Transaction string
	BundleRoot  string
}

const (
	MaxChangeEntries = 100_000
)

func validatePath(value string) error {
	if value == "" || strings.HasPrefix(value, "/") || path.Clean(value) != value ||
		value == "." || value == ".." || strings.HasPrefix(value, "../") || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("unsafe change path %q", value)
	}
	if pathContainsGitMetadata(value) {
		return fmt.Errorf("change path %q targets Git metadata", value)
	}
	return nil
}

func pathContainsGitMetadata(value string) bool {
	for _, component := range strings.Split(filepath.ToSlash(value), "/") {
		if strings.EqualFold(component, ".git") {
			return true
		}
	}
	return false
}

func captureBaseline(root, rel string) (Baseline, error) {
	return captureBaselineContext(context.Background(), root, rel)
}

func captureBaselineContext(ctx context.Context, root, rel string) (Baseline, error) {
	return captureBaselineAtContext(ctx, root, rel, rel)
}

func captureBaselineAt(root, rel, logicalPath string) (Baseline, error) {
	return captureBaselineAtContext(context.Background(), root, rel, logicalPath)
}

func captureBaselineAtContext(ctx context.Context, root, rel, logicalPath string) (Baseline, error) {
	if err := ctx.Err(); err != nil {
		return Baseline{}, err
	}
	if err := validatePath(rel); err != nil {
		return Baseline{}, err
	}
	if err := validatePath(logicalPath); err != nil {
		return Baseline{}, err
	}
	destination, err := os.OpenRoot(root)
	if err != nil {
		return Baseline{}, err
	}
	defer destination.Close()
	baseline, _, _, err := captureBaselineAtRootBoundedContext(ctx, destination, rel, logicalPath, -1, -1)
	return baseline, err
}

func captureBaselineAtRootContext(ctx context.Context, root *os.Root, rel, logicalPath string) (Baseline, error) {
	baseline, _, _, err := captureBaselineAtRootBoundedContext(
		ctx, root, rel, logicalPath, proto.DefaultLimits().MaxChangeBytes, MaxChangeEntries,
	)
	return baseline, err
}

func captureBaselineAtRootStrictContext(ctx context.Context, root *os.Root, rel, logicalPath string) (Baseline, error) {
	manifest, missing, _, _, err := captureManifestAtRootBoundedContext(
		ctx, root, rel, logicalPath, proto.DefaultLimits().MaxChangeBytes, MaxChangeEntries,
	)
	if err != nil {
		return Baseline{}, err
	}
	if missing {
		return Baseline{Path: logicalPath, Missing: true}, nil
	}
	return Baseline{Path: logicalPath, Digest: manifest.RootHash()}, nil
}

func captureBaselineAtRootBoundedContext(
	ctx context.Context,
	root *os.Root,
	rel string,
	logicalPath string,
	maxBytes int64,
	maxEntries int,
) (Baseline, int64, int, error) {
	manifest, missing, logicalBytes, entries, err := captureManifestAtRootAccessibleBoundedContext(
		ctx, root, rel, logicalPath, maxBytes, maxEntries,
	)
	if err != nil {
		return Baseline{}, 0, 0, err
	}
	if missing {
		return Baseline{Path: logicalPath, Missing: true}, 0, 0, nil
	}
	return Baseline{Path: logicalPath, Digest: manifest.RootHash()}, logicalBytes, entries, nil
}

func captureManifestAtRootAccessibleBoundedContext(
	ctx context.Context,
	root *os.Root,
	rel string,
	logicalPath string,
	maxBytes int64,
	maxEntries int,
) (proto.Manifest, bool, int64, int, error) {
	if _, err := root.Lstat(rel); err != nil {
		if os.IsNotExist(err) {
			return proto.Manifest{}, true, 0, 0, nil
		}
		return proto.Manifest{}, false, 0, 0, err
	}
	access, err := makeSubtreeAccessibleAtRootContext(ctx, root, rel, maxEntries, maxBytes)
	if err != nil {
		return proto.Manifest{}, false, 0, 0, err
	}
	manifest, missing, logicalBytes, entries, captureErr := captureManifestAtRootBoundedContext(
		ctx, root, rel, logicalPath, maxBytes, maxEntries,
	)
	access.logicalizeRebased(&manifest, rel, logicalPath)
	return manifest, missing, logicalBytes, entries, errors.Join(captureErr, access.restore())
}

func captureManifestAtRootBoundedContext(
	ctx context.Context,
	root *os.Root,
	rel string,
	logicalPath string,
	maxBytes int64,
	maxEntries int,
) (proto.Manifest, bool, int64, int, error) {
	if err := ctx.Err(); err != nil {
		return proto.Manifest{}, false, 0, 0, err
	}
	if err := validatePath(rel); err != nil {
		return proto.Manifest{}, false, 0, 0, err
	}
	if err := validatePath(logicalPath); err != nil {
		return proto.Manifest{}, false, 0, 0, err
	}
	if err := rejectSymlinkParentsAtRoot(root, rel); err != nil {
		return proto.Manifest{}, false, 0, 0, err
	}
	if _, err := root.Lstat(rel); err != nil {
		if os.IsNotExist(err) {
			return proto.Manifest{}, true, 0, 0, nil
		}
		return proto.Manifest{}, false, 0, 0, err
	}
	var paths []string
	var logicalBytes int64
	var walk func(string) error
	walk = func(current string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if maxEntries >= 0 && len(paths) >= maxEntries {
			return fmt.Errorf("%w: change baseline exceeds %d entries", ErrEntryLimitExceeded, maxEntries)
		}
		if info.Mode().IsRegular() {
			if maxBytes >= 0 && info.Size() > maxBytes-logicalBytes {
				return fmt.Errorf("%w: change baseline exceeds %d bytes", ErrByteLimitExceeded, maxBytes)
			}
			logicalBytes += info.Size()
		}
		paths = append(paths, filepath.ToSlash(current))
		if !info.IsDir() {
			return nil
		}
		dir, err := root.Open(current)
		if err != nil {
			return err
		}
		for {
			entries, readErr := dir.ReadDir(256)
			for _, entry := range entries {
				if err := walk(path.Join(current, entry.Name())); err != nil {
					dir.Close()
					return err
				}
			}
			if readErr == io.EOF {
				return dir.Close()
			}
			if readErr != nil {
				return errors.Join(readErr, dir.Close())
			}
		}
	}
	if err := walk(rel); err != nil {
		return proto.Manifest{}, false, 0, 0, err
	}
	sort.Strings(paths)
	manifest := proto.Manifest{Entries: make([]proto.ManifestEntry, 0, len(paths))}
	for _, current := range paths {
		if err := ctx.Err(); err != nil {
			return proto.Manifest{}, false, 0, 0, err
		}
		info, err := root.Lstat(current)
		if err != nil {
			return proto.Manifest{}, false, 0, 0, err
		}
		entry := proto.ManifestEntry{Path: current, Mode: uint32(info.Mode().Perm())}
		switch {
		case info.Mode().IsRegular():
			entry.Type = proto.EntryFile
			entry.Size = info.Size()
			entry.SHA256, err = hashRootFileContext(ctx, root, current, info)
		case info.IsDir():
			entry.Type = proto.EntryDir
		case info.Mode()&fs.ModeSymlink != 0:
			entry.Type = proto.EntrySymlink
			entry.Target, err = root.Readlink(current)
		default:
			err = fmt.Errorf("unsupported change type %v at %s", info.Mode(), current)
		}
		if err != nil {
			return proto.Manifest{}, false, 0, 0, err
		}
		manifest.Entries = append(manifest.Entries, entry)
	}
	manifest = rebaseManifest(manifest, rel, logicalPath)
	return manifest, false, logicalBytes, len(paths), nil
}

func hashRootFileContext(ctx context.Context, root *os.Root, rel string, before os.FileInfo) (string, error) {
	beforeIdentity, err := fsidentity.FromInfo(before)
	if err != nil {
		return "", err
	}
	f, err := root.Open(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return "", err
	}
	openedIdentity, err := fsidentity.FromInfo(opened)
	if err != nil || !opened.Mode().IsRegular() || openedIdentity != beforeIdentity ||
		opened.Size() != before.Size() || opened.Mode() != before.Mode() {
		return "", fmt.Errorf("change %q changed while it was being inspected", rel)
	}
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	var read int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, readErr := f.Read(buffer)
		if n > 0 {
			read += int64(n)
			if _, err := hash.Write(buffer[:n]); err != nil {
				return "", err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	after, err := f.Stat()
	if err != nil {
		return "", err
	}
	afterIdentity, identityErr := fsidentity.FromInfo(after)
	namedAfter, namedErr := root.Lstat(rel)
	var namedIdentity fsidentity.Identity
	if namedErr == nil {
		namedIdentity, namedErr = fsidentity.FromInfo(namedAfter)
	}
	if identityErr != nil || namedErr != nil || read != before.Size() ||
		afterIdentity != beforeIdentity || namedIdentity != beforeIdentity ||
		after.Size() != before.Size() || after.Mode() != before.Mode() ||
		namedAfter.Size() != before.Size() || namedAfter.Mode() != before.Mode() {
		return "", fmt.Errorf("change %q changed while it was being inspected", rel)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// CollectWorkspaceChangesContext retains the submitted and final values of
// every changed root so a later client can perform a real three-way merge.
func CollectWorkspaceChangesContext(
	ctx context.Context,
	workspace string,
	jobDir string,
	baseline proto.Manifest,
	selection proto.SelectionPolicy,
	maxBytes int64,
) (proto.ChangeBundle, bool, error) {
	if err := ctx.Err(); err != nil {
		return proto.ChangeBundle{}, false, err
	}
	entryLimit := len(baseline.Entries) + MaxChangeEntries
	byteLimit, err := workspaceCollectionByteLimit(baseline, maxBytes)
	if err != nil {
		return proto.ChangeBundle{}, false, err
	}
	selector, err := newRetentionSelector(baseline, selection)
	if err != nil {
		return proto.ChangeBundle{}, false, fmt.Errorf("compiling selection policy: %w", err)
	}
	access, err := makeTreeAccessibleFilteredContext(ctx, workspace, entryLimit, byteLimit, selector.selectPath)
	if err != nil {
		return proto.ChangeBundle{}, false, err
	}
	bundle, collected, collectErr := collectAccessibleWorkspaceChangesContext(
		ctx, workspace, jobDir, baseline, maxBytes, access, selector,
	)
	return bundle, collected, errors.Join(collectErr, access.restore())
}

func collectAccessibleWorkspaceChangesContext(
	ctx context.Context,
	workspace string,
	jobDir string,
	baseline proto.Manifest,
	maxBytes int64,
	access *treeAccess,
	selector *retentionSelector,
) (proto.ChangeBundle, bool, error) {
	entryLimit := len(baseline.Entries) + MaxChangeEntries
	byteLimit, err := workspaceCollectionByteLimit(baseline, maxBytes)
	if err != nil {
		return proto.ChangeBundle{}, false, err
	}
	paths, err := workspacePathsForBaselineContext(ctx, workspace, baseline, entryLimit, byteLimit, selector)
	if err != nil {
		return proto.ChangeBundle{}, false, err
	}
	current, err := snapshot.BuildBoundedContext(ctx, workspace, paths, byteLimit, entryLimit)
	if err != nil {
		switch {
		case errors.Is(err, snapshot.ErrEntryLimitExceeded):
			return proto.ChangeBundle{}, false, fmt.Errorf("%w: too many workspace entries", ErrEntryLimitExceeded)
		case errors.Is(err, snapshot.ErrByteLimitExceeded):
			return proto.ChangeBundle{}, false, fmt.Errorf("%w: workspace changes exceed byte limit", ErrByteLimitExceeded)
		case errors.Is(err, snapshot.ErrLimitExceeded):
			return proto.ChangeBundle{}, false, fmt.Errorf("%w: workspace changes exceed limit", ErrLimitExceeded)
		}
		return proto.ChangeBundle{}, false, err
	}
	access.logicalize(&current)
	bundle, err := workspaceDelta(ctx, baseline, current, maxBytes)
	if err != nil || len(bundle.Paths) == 0 {
		return bundle, false, err
	}
	if err := commitBundleWithPhysicalModesContext(
		ctx, workspaceBasePath(jobDir), workspace, jobDir, bundle, nil, access.physical,
	); err != nil {
		return proto.ChangeBundle{}, false, err
	}
	return bundle, true, nil
}

func workspaceCollectionByteLimit(baseline proto.Manifest, maxBytes int64) (int64, error) {
	if maxBytes < 0 {
		return -1, nil
	}
	var baselineBytes int64
	for _, entry := range baseline.Entries {
		if entry.Type != proto.EntryFile {
			continue
		}
		if entry.Size > math.MaxInt64-baselineBytes {
			return 0, fmt.Errorf("%w: workspace byte limit overflows", ErrByteLimitExceeded)
		}
		baselineBytes += entry.Size
	}
	if maxBytes > math.MaxInt64-baselineBytes {
		return 0, fmt.Errorf("%w: workspace byte limit overflows", ErrByteLimitExceeded)
	}
	return baselineBytes + maxBytes, nil
}

func workspacePathsContext(ctx context.Context, root string, maxEntries int, maxBytes int64) ([]string, error) {
	selector, err := newRetentionSelector(proto.Manifest{}, proto.SelectionPolicy{})
	if err != nil {
		return nil, err
	}
	return workspacePathsForBaselineContext(ctx, root, proto.Manifest{}, maxEntries, maxBytes, selector)
}

func workspacePathsForBaselineContext(
	ctx context.Context,
	root string,
	baseline proto.Manifest,
	maxEntries int,
	maxBytes int64,
	selector *retentionSelector,
) ([]string, error) {
	var paths []string
	var bytes int64
	baselinePaths := make(map[string]struct{}, len(baseline.Entries))
	for _, entry := range baseline.Entries {
		baselinePaths[entry.Path] = struct{}{}
	}
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if current == root {
			return nil
		}
		rel, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		include, descend := selector.selectPath(rel, info)
		if !descend && entry.IsDir() {
			return fs.SkipDir
		}
		if !include {
			return nil
		}
		if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&fs.ModeSymlink == 0 {
			if _, submitted := baselinePaths[rel]; submitted {
				return fmt.Errorf("unsupported workspace node %q replaces submitted path", rel)
			}
			return nil
		}
		if maxEntries >= 0 && len(paths) >= maxEntries {
			return fmt.Errorf("%w: workspace exceeds %d entries", ErrEntryLimitExceeded, maxEntries)
		}
		if info.Mode().IsRegular() {
			if maxBytes >= 0 && info.Size() > maxBytes-bytes {
				return fmt.Errorf("%w: workspace exceeds %d bytes", ErrByteLimitExceeded, maxBytes)
			}
			bytes += info.Size()
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func workspaceDelta(ctx context.Context, baseline, current proto.Manifest, maxBytes int64) (proto.ChangeBundle, error) {
	if err := ctx.Err(); err != nil {
		return proto.ChangeBundle{}, err
	}
	before, err := manifestEntriesByPath(ctx, baseline)
	if err != nil {
		return proto.ChangeBundle{}, err
	}
	after, err := manifestEntriesByPath(ctx, current)
	if err != nil {
		return proto.ChangeBundle{}, err
	}
	changed := make([]string, 0)
	for name, entry := range before {
		if err := ctx.Err(); err != nil {
			return proto.ChangeBundle{}, err
		}
		if got, ok := after[name]; !ok || got != entry {
			changed = append(changed, name)
		}
	}
	for name, entry := range after {
		if err := ctx.Err(); err != nil {
			return proto.ChangeBundle{}, err
		}
		if got, ok := before[name]; !ok || got != entry {
			if _, existed := before[name]; !existed {
				changed = append(changed, name)
			}
		}
	}
	sort.Strings(changed)
	metadataOnly := make(map[string]struct{})
	for _, candidate := range changed {
		beforeEntry, beforeOK := before[candidate]
		afterEntry, afterOK := after[candidate]
		if !beforeOK || !afterOK || beforeEntry.Type != proto.EntryDir || afterEntry.Type != proto.EntryDir {
			continue
		}
		metadataOnly[candidate] = struct{}{}
	}
	roots := make([]string, 0, len(changed))
	contentRoots := make(map[string]struct{}, len(changed))
	for _, candidate := range changed {
		if err := ctx.Err(); err != nil {
			return proto.ChangeBundle{}, err
		}
		_, metadata := metadataOnly[candidate]
		if !metadata {
			covered := false
			for parent := path.Dir(candidate); parent != "."; parent = path.Dir(parent) {
				if _, ok := contentRoots[parent]; ok {
					covered = true
					break
				}
			}
			if covered {
				continue
			}
		}
		roots = append(roots, candidate)
		if !metadata {
			contentRoots[candidate] = struct{}{}
		}
	}
	if len(roots) > MaxChangeEntries {
		return proto.ChangeBundle{}, fmt.Errorf("%w: changes exceed %d paths", ErrEntryLimitExceeded, MaxChangeEntries)
	}

	bundle := proto.ChangeBundle{
		V: BundleVersion, BaselineRoot: baseline.RootHash(), Paths: roots,
	}
	baseSelected := make(map[string]proto.ManifestEntry)
	remoteSelected := make(map[string]proto.ManifestEntry)
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return proto.ChangeBundle{}, err
		}
		if _, metadata := metadataOnly[root]; metadata {
			bundle.MetadataPaths = append(bundle.MetadataPaths, root)
			baseSelected[root] = before[root]
			remoteSelected[root] = after[root]
		} else {
			original := subtreeManifest(baseline, root)
			final := subtreeManifest(current, root)
			selectManifestEntries(baseSelected, before, original)
			selectManifestEntries(remoteSelected, after, final)
		}
		selectManifestAncestors(baseSelected, before, root)
		selectManifestAncestors(remoteSelected, after, root)
	}
	bundle.BaseManifest, bundle.Bytes, err = selectedManifest(ctx, baseSelected, maxBytes, bundle.Bytes)
	if err != nil {
		return proto.ChangeBundle{}, err
	}
	bundle.RemoteManifest, bundle.Bytes, err = selectedManifest(ctx, remoteSelected, maxBytes, bundle.Bytes)
	if err != nil {
		return proto.ChangeBundle{}, err
	}
	return bundle, nil
}

func selectManifestAncestors(
	selected map[string]proto.ManifestEntry,
	all map[string]proto.ManifestEntry,
	entryPath string,
) {
	for parent := path.Dir(entryPath); parent != "."; parent = path.Dir(parent) {
		if ancestor, ok := all[parent]; ok {
			selected[parent] = ancestor
		}
	}
}

func selectManifestEntries(
	selected map[string]proto.ManifestEntry,
	all map[string]proto.ManifestEntry,
	subtree proto.Manifest,
) {
	for _, entry := range subtree.Entries {
		selected[entry.Path] = entry
		for parent := path.Dir(entry.Path); parent != "."; parent = path.Dir(parent) {
			if ancestor, ok := all[parent]; ok {
				selected[parent] = ancestor
			}
		}
	}
}

func selectedManifest(
	ctx context.Context,
	selected map[string]proto.ManifestEntry,
	maxBytes int64,
	initialBytes int64,
) (proto.Manifest, int64, error) {
	paths := make([]string, 0, len(selected))
	for name := range selected {
		if err := ctx.Err(); err != nil {
			return proto.Manifest{}, 0, err
		}
		paths = append(paths, name)
	}
	sort.Strings(paths)
	manifest := proto.Manifest{Entries: make([]proto.ManifestEntry, 0, len(paths))}
	bytes := initialBytes
	for _, name := range paths {
		if err := ctx.Err(); err != nil {
			return proto.Manifest{}, 0, err
		}
		entry := selected[name]
		manifest.Entries = append(manifest.Entries, entry)
		if entry.Type == proto.EntryFile {
			if maxBytes >= 0 && entry.Size > maxBytes-bytes {
				return proto.Manifest{}, 0, fmt.Errorf("%w: workspace changes exceed %d bytes", ErrByteLimitExceeded, maxBytes)
			}
			bytes += entry.Size
		}
	}
	return manifest, bytes, nil
}

func manifestEntriesByPath(ctx context.Context, manifest proto.Manifest) (map[string]proto.ManifestEntry, error) {
	entries := make(map[string]proto.ManifestEntry, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries[entry.Path] = entry
	}
	return entries, nil
}

func subtreeManifest(manifest proto.Manifest, root string) proto.Manifest {
	exact := sort.Search(len(manifest.Entries), func(i int) bool {
		return manifest.Entries[i].Path >= root
	})
	prefix := root + "/"
	start := sort.Search(len(manifest.Entries), func(i int) bool {
		return manifest.Entries[i].Path >= prefix
	})
	end := start
	for end < len(manifest.Entries) {
		if !strings.HasPrefix(manifest.Entries[end].Path, prefix) {
			break
		}
		end++
	}
	hasExact := exact < len(manifest.Entries) && manifest.Entries[exact].Path == root
	if !hasExact {
		return proto.Manifest{Entries: manifest.Entries[start:end]}
	}
	if start == exact+1 {
		return proto.Manifest{Entries: manifest.Entries[exact:end]}
	}
	entries := make([]proto.ManifestEntry, 0, 1+end-start)
	entries = append(entries, manifest.Entries[exact])
	entries = append(entries, manifest.Entries[start:end]...)
	return proto.Manifest{Entries: entries}
}

func rejectSymlinkParents(root, rel string) error {
	parts := strings.Split(filepath.FromSlash(rel), string(filepath.Separator))
	current := root
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("change path %q passes through symlink %q", rel, current)
		}
		if !info.IsDir() {
			return fmt.Errorf("change path %q passes through non-directory %q", rel, current)
		}
	}
	return nil
}

func rejectSymlinkParentsAtRoot(root *os.Root, rel string) error {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i := 1; i < len(parts); i++ {
		current := strings.Join(parts[:i], "/")
		info, err := root.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("change path %q passes through symlink %q", rel, current)
		}
		if !info.IsDir() {
			return fmt.Errorf("change path %q passes through non-directory %q", rel, current)
		}
	}
	return nil
}
