package changes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

type MergeConflictError struct {
	Paths []string
}

func (e *MergeConflictError) Error() string {
	return "workspace changes conflict at: " + strings.Join(e.Paths, ", ")
}

type mergeTree struct {
	root     string
	entries  map[string]proto.ManifestEntry
	children map[string][]string
}

type treeDelta map[string]bool

type truncatingBuffer struct {
	bytes.Buffer
	remaining int
}

func (w *truncatingBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if len(p) > w.remaining {
		p = p[:w.remaining]
	}
	if len(p) != 0 {
		_, _ = w.Buffer.Write(p)
		w.remaining -= len(p)
	}
	return written, nil
}

func mergeChangeRoots(
	ctx context.Context,
	baseRoot string,
	oursRoot string,
	remoteRoot string,
	mergedRoot string,
	bundle proto.ChangeBundle,
	paths []string,
	ours proto.Manifest,
) error {
	base := newMergeTree(baseRoot, bundle.BaseManifest)
	local := newMergeTree(oursRoot, ours)
	remote := newMergeTree(remoteRoot, bundle.RemoteManifest)
	baseLocal := changedSubtrees(base.entries, local.entries)
	baseRemote := changedSubtrees(base.entries, remote.entries)
	localRemote := changedSubtrees(local.entries, remote.entries)

	conflicts := make(map[string]bool)
	metadata := make(map[string]bool, len(bundle.MetadataPaths))
	for _, metadataPath := range bundle.MetadataPaths {
		metadata[metadataPath] = true
	}
	for _, changePath := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if metadata[changePath] {
			if err := mergeMetadataPath(changePath, mergedRoot, base, local, remote, conflicts); err != nil {
				return err
			}
			continue
		}
		if err := mergeTreePath(
			ctx, changePath, mergedRoot, base, local, remote,
			baseLocal, baseRemote, localRemote, conflicts,
		); err != nil {
			return err
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	conflictPaths := make([]string, 0, len(conflicts))
	for conflictPath := range conflicts {
		conflictPaths = append(conflictPaths, conflictPath)
	}
	sort.Strings(conflictPaths)
	return &MergeConflictError{Paths: conflictPaths}
}

func mergeMetadataPath(name, mergedRoot string, base, ours, remote mergeTree, conflicts map[string]bool) error {
	baseEntry, baseOK := base.entries[name]
	oursEntry, oursOK := ours.entries[name]
	remoteEntry, remoteOK := remote.entries[name]
	if !baseOK || !oursOK || !remoteOK || baseEntry.Type != proto.EntryDir ||
		oursEntry.Type != proto.EntryDir || remoteEntry.Type != proto.EntryDir {
		conflicts[name] = true
		return nil
	}
	mode, ok := mergeMode(baseEntry.Mode, true, oursEntry.Mode, remoteEntry.Mode)
	if !ok {
		conflicts[name] = true
		return nil
	}
	dest := filepath.Join(mergedRoot, filepath.FromSlash(name))
	if err := os.MkdirAll(dest, os.FileMode(mode)|0o700); err != nil {
		return err
	}
	return os.Chmod(dest, os.FileMode(mode))
}

func newMergeTree(root string, manifest proto.Manifest) mergeTree {
	entries := make(map[string]proto.ManifestEntry, len(manifest.Entries))
	children := make(map[string][]string)
	for _, entry := range manifest.Entries {
		entries[entry.Path] = entry
		parent := path.Dir(entry.Path)
		children[parent] = append(children[parent], entry.Path)
	}
	return mergeTree{root: root, entries: entries, children: children}
}

func changedSubtrees(left, right map[string]proto.ManifestEntry) treeDelta {
	changed := make(treeDelta)
	mark := func(name string) {
		for current := name; current != "."; current = path.Dir(current) {
			changed[current] = true
		}
	}
	for name, entry := range left {
		if other, ok := right[name]; !ok || other != entry {
			mark(name)
		}
	}
	for name, entry := range right {
		if other, ok := left[name]; !ok || other != entry {
			mark(name)
		}
	}
	return changed
}

func mergeTreePath(
	ctx context.Context,
	name string,
	mergedRoot string,
	base mergeTree,
	ours mergeTree,
	remote mergeTree,
	baseOurs treeDelta,
	baseRemote treeDelta,
	oursRemote treeDelta,
	conflicts map[string]bool,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch {
	case !oursRemote[name]:
		return copyMergeSubtree(ours, name, mergedRoot)
	case !baseOurs[name]:
		return copyMergeSubtree(remote, name, mergedRoot)
	case !baseRemote[name]:
		return copyMergeSubtree(ours, name, mergedRoot)
	}

	baseEntry, baseOK := base.entries[name]
	oursEntry, oursOK := ours.entries[name]
	remoteEntry, remoteOK := remote.entries[name]
	if oursOK && remoteOK && oursEntry.Type == proto.EntryDir && remoteEntry.Type == proto.EntryDir &&
		(!baseOK || baseEntry.Type == proto.EntryDir) {
		mode, ok := mergeMode(baseEntry.Mode, baseOK, oursEntry.Mode, remoteEntry.Mode)
		if !ok {
			conflicts[name] = true
			return nil
		}
		dest := filepath.Join(mergedRoot, filepath.FromSlash(name))
		if err := os.MkdirAll(dest, os.FileMode(mode)|0o700); err != nil {
			return err
		}
		for _, child := range mergeChildren(name, base, ours, remote) {
			if err := mergeTreePath(
				ctx, child, mergedRoot, base, ours, remote,
				baseOurs, baseRemote, oursRemote, conflicts,
			); err != nil {
				return err
			}
		}
		return os.Chmod(dest, os.FileMode(mode))
	}
	if baseOK && oursOK && remoteOK &&
		baseEntry.Type == proto.EntryFile && oursEntry.Type == proto.EntryFile && remoteEntry.Type == proto.EntryFile {
		mode, ok := mergeMode(baseEntry.Mode, true, oursEntry.Mode, remoteEntry.Mode)
		if !ok {
			conflicts[name] = true
			return nil
		}
		clean, err := mergeRegularFile(
			ctx,
			filepath.Join(ours.root, filepath.FromSlash(name)),
			filepath.Join(base.root, filepath.FromSlash(name)),
			filepath.Join(remote.root, filepath.FromSlash(name)),
			filepath.Join(mergedRoot, filepath.FromSlash(name)),
			os.FileMode(mode),
		)
		if err != nil {
			return err
		}
		if !clean {
			conflicts[name] = true
		}
		return nil
	}
	if baseOK && oursOK && remoteOK &&
		baseEntry.Type == proto.EntrySymlink && oursEntry.Type == proto.EntrySymlink && remoteEntry.Type == proto.EntrySymlink {
		target, targetOK := mergeString(baseEntry.Target, oursEntry.Target, remoteEntry.Target)
		_, modeOK := mergeMode(baseEntry.Mode, true, oursEntry.Mode, remoteEntry.Mode)
		if !targetOK || !modeOK {
			conflicts[name] = true
			return nil
		}
		dest := filepath.Join(mergedRoot, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		return os.Symlink(target, dest)
	}
	conflicts[name] = true
	return nil
}

func mergeChildren(parent string, trees ...mergeTree) []string {
	children := make(map[string]bool)
	for _, tree := range trees {
		for _, name := range tree.children[parent] {
			children[name] = true
		}
	}
	result := make([]string, 0, len(children))
	for name := range children {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func mergeMode(base uint32, baseOK bool, ours, remote uint32) (uint32, bool) {
	if ours == remote {
		return ours, true
	}
	if !baseOK {
		return 0, false
	}
	if ours == base {
		return remote, true
	}
	if remote == base {
		return ours, true
	}
	return 0, false
}

func mergeString(base, ours, remote string) (string, bool) {
	if ours == remote {
		return ours, true
	}
	if ours == base {
		return remote, true
	}
	if remote == base {
		return ours, true
	}
	return "", false
}

func copyMergeSubtree(source mergeTree, name, mergedRoot string) error {
	if _, ok := source.entries[name]; !ok {
		return nil
	}
	dest := filepath.Join(mergedRoot, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	if err := copyPath(filepath.Join(source.root, filepath.FromSlash(name)), dest); err != nil {
		return err
	}
	return restoreMergeSubtreeModes(source, name, mergedRoot)
}

func restoreMergeSubtreeModes(source mergeTree, name, mergedRoot string) error {
	prefix := name + "/"
	var paths []string
	for entryPath := range source.entries {
		if entryPath == name || strings.HasPrefix(entryPath, prefix) {
			paths = append(paths, entryPath)
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		leftDepth := strings.Count(paths[i], "/")
		rightDepth := strings.Count(paths[j], "/")
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		return paths[i] > paths[j]
	})
	for _, entryPath := range paths {
		entry := source.entries[entryPath]
		if entry.Type == proto.EntrySymlink {
			continue
		}
		if err := os.Chmod(
			filepath.Join(mergedRoot, filepath.FromSlash(entryPath)),
			os.FileMode(entry.Mode),
		); err != nil {
			return err
		}
	}
	return nil
}

func mergeRegularFile(ctx context.Context, ours, base, remote, dest string, mode os.FileMode) (bool, error) {
	for _, name := range []string{ours, base, remote} {
		binary, err := isBinaryFile(name)
		if err != nil {
			return false, err
		}
		if binary {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return false, err
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	cmd := exec.CommandContext(ctx, "git", "merge-file", "--stdout", ours, base, remote)
	cmd.Stdout = out
	stderr := truncatingBuffer{remaining: 32 << 10}
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	closeErr := out.Close()
	if runErr != nil {
		_ = os.Remove(dest)
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			if code := exitErr.ExitCode(); code > 0 && code < 128 {
				return false, closeErr
			}
			detail := strings.TrimSpace(stderr.String())
			if detail != "" {
				runErr = fmt.Errorf("git merge-file failed: %s: %w", detail, runErr)
			}
		}
		return false, errors.Join(runErr, closeErr)
	}
	if closeErr != nil {
		_ = os.Remove(dest)
		return false, closeErr
	}
	if err := os.Chmod(dest, mode.Perm()); err != nil {
		_ = os.Remove(dest)
		return false, err
	}
	return true, nil
}

func isBinaryFile(name string) (bool, error) {
	f, err := os.Open(name)
	if err != nil {
		return false, err
	}
	defer f.Close()
	buffer := make([]byte, 8<<10)
	n, err := f.Read(buffer)
	if err != nil && err != io.EOF {
		return false, err
	}
	return bytes.IndexByte(buffer[:n], 0) >= 0, nil
}
