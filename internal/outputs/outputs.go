// Package outputs owns declared output validation, durable target collection,
// local conflict baselines, verified extraction, and conflict-safe application.
package outputs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/lydakis/errand/internal/snapshot"
)

type Baseline struct {
	Path    string `json:"path"`
	Digest  string `json:"digest,omitempty"`
	Missing bool   `json:"missing,omitempty"`
}

type ApplyResult struct {
	Applied     []string
	Transaction string
	BundleRoot  string
}

const (
	MaxOutputEntries = 100_000
	MaxOutputPaths   = 256
)

func NormalizeSpecs(specs []proto.OutputSpec) ([]proto.OutputSpec, error) {
	if len(specs) > MaxOutputPaths {
		return nil, fmt.Errorf("output declarations exceed %d paths", MaxOutputPaths)
	}
	normalized := append([]proto.OutputSpec(nil), specs...)
	for i := range normalized {
		spec := &normalized[i]
		if err := validatePath(spec.Path); err != nil {
			return nil, err
		}
		if spec.Collect == "" {
			spec.Collect = proto.OutputCollectSuccess
		}
		if spec.Apply == "" {
			spec.Apply = proto.OutputApplyManual
		}
		if spec.Collect != proto.OutputCollectSuccess && spec.Collect != proto.OutputCollectAlways {
			return nil, fmt.Errorf("output %q has invalid collect mode %q", spec.Path, spec.Collect)
		}
		if spec.Apply != proto.OutputApplyAuto && spec.Apply != proto.OutputApplyManual {
			return nil, fmt.Errorf("output %q has invalid apply mode %q", spec.Path, spec.Apply)
		}
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].Path < normalized[j].Path })
	seen := make(map[string]string, len(normalized))
	for _, spec := range normalized {
		folded := strings.ToLower(spec.Path)
		if prior, ok := seen[folded]; ok {
			if prior != spec.Path {
				return nil, fmt.Errorf("output paths %q and %q collide on case-insensitive filesystems", prior, spec.Path)
			}
			return nil, fmt.Errorf("output path %q is declared more than once", spec.Path)
		}
		seen[folded] = spec.Path
	}
	for _, spec := range normalized {
		for parent := path.Dir(strings.ToLower(spec.Path)); parent != "."; parent = path.Dir(parent) {
			if prior, ok := seen[parent]; ok {
				return nil, fmt.Errorf("output paths %q and %q overlap", prior, spec.Path)
			}
		}
	}
	return normalized, nil
}

func validatePath(value string) error {
	if value == "" || strings.HasPrefix(value, "/") || path.Clean(value) != value ||
		value == "." || value == ".." || strings.HasPrefix(value, "../") || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("unsafe output path %q", value)
	}
	if pathContainsGitMetadata(value) {
		return fmt.Errorf("output path %q targets Git metadata", value)
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

func CaptureBaselines(root string, specs []proto.OutputSpec) ([]Baseline, error) {
	return CaptureBaselinesContext(context.Background(), root, specs)
}

func CaptureBaselinesContext(ctx context.Context, root string, specs []proto.OutputSpec) ([]Baseline, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	normalized, err := NormalizeSpecs(specs)
	if err != nil {
		return nil, err
	}
	baselines := make([]Baseline, 0, len(normalized))
	for _, spec := range normalized {
		baseline, err := captureBaselineContext(ctx, root, spec.Path)
		if err != nil {
			return nil, err
		}
		baselines = append(baselines, baseline)
	}
	return baselines, nil
}

// CaptureWorkspaceBaselinesContext pins root while it reads the declared
// outputs and returns the identity that must guard every later apply operation.
func CaptureWorkspaceBaselinesContext(
	ctx context.Context,
	root string,
	specs []proto.OutputSpec,
	maxBytes int64,
	maxEntries int,
) ([]Baseline, fsidentity.Identity, error) {
	if err := ctx.Err(); err != nil {
		return nil, fsidentity.Identity{}, err
	}
	normalized, err := NormalizeSpecs(specs)
	if err != nil {
		return nil, fsidentity.Identity{}, err
	}
	destination, err := openApplyDestination(root)
	if err != nil {
		return nil, fsidentity.Identity{}, err
	}
	defer destination.Close()
	baselines := make([]Baseline, 0, len(normalized))
	remainingBytes := maxBytes
	remainingEntries := maxEntries
	for _, spec := range normalized {
		baseline, bytes, entries, err := captureBaselineAtRootBoundedContext(
			ctx, destination.root, spec.Path, spec.Path, remainingBytes, remainingEntries,
		)
		if err != nil {
			return nil, fsidentity.Identity{}, err
		}
		baselines = append(baselines, baseline)
		if remainingBytes >= 0 {
			remainingBytes -= bytes
		}
		if remainingEntries >= 0 {
			remainingEntries -= entries
		}
	}
	if err := destination.verifyPath(); err != nil {
		return nil, fsidentity.Identity{}, err
	}
	return baselines, destination.identity, nil
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
	if err := rejectSymlinkParents(root, rel); err != nil {
		return Baseline{}, err
	}
	paths, missing, _, err := declaredPathsBoundedContext(ctx, root, rel, -1, -1)
	if err != nil {
		return Baseline{}, err
	}
	if missing {
		return Baseline{Path: logicalPath, Missing: true}, nil
	}
	manifest, err := snapshot.BuildBoundedContext(ctx, root, paths, -1, -1)
	if err != nil {
		return Baseline{}, err
	}
	manifest = subtreeManifest(manifest, rel)
	if err := snapshot.PackContext(ctx, io.Discard, root, manifest); err != nil {
		return Baseline{}, err
	}
	manifest = rebaseManifest(manifest, rel, logicalPath)
	return Baseline{Path: logicalPath, Digest: manifest.RootHash()}, nil
}

func captureBaselineAtRootContext(ctx context.Context, root *os.Root, rel, logicalPath string) (Baseline, error) {
	baseline, _, _, err := captureBaselineAtRootBoundedContext(
		ctx, root, rel, logicalPath, proto.DefaultLimits().MaxOutputBytes, MaxOutputEntries,
	)
	return baseline, err
}

func captureBaselineAtRootBoundedContext(
	ctx context.Context,
	root *os.Root,
	rel string,
	logicalPath string,
	maxBytes int64,
	maxEntries int,
) (Baseline, int64, int, error) {
	if err := ctx.Err(); err != nil {
		return Baseline{}, 0, 0, err
	}
	if err := validatePath(rel); err != nil {
		return Baseline{}, 0, 0, err
	}
	if err := validatePath(logicalPath); err != nil {
		return Baseline{}, 0, 0, err
	}
	if err := rejectSymlinkParentsAtRoot(root, rel); err != nil {
		return Baseline{}, 0, 0, err
	}
	if _, err := root.Lstat(rel); err != nil {
		if os.IsNotExist(err) {
			return Baseline{Path: logicalPath, Missing: true}, 0, 0, nil
		}
		return Baseline{}, 0, 0, err
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
			return fmt.Errorf("%w: output baseline exceeds %d entries", ErrLimitExceeded, maxEntries)
		}
		if info.Mode().IsRegular() {
			if maxBytes >= 0 && info.Size() > maxBytes-logicalBytes {
				return fmt.Errorf("%w: output baseline exceeds %d bytes", ErrLimitExceeded, maxBytes)
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
		return Baseline{}, 0, 0, err
	}
	sort.Strings(paths)
	manifest := proto.Manifest{Entries: make([]proto.ManifestEntry, 0, len(paths))}
	for _, current := range paths {
		if err := ctx.Err(); err != nil {
			return Baseline{}, 0, 0, err
		}
		info, err := root.Lstat(current)
		if err != nil {
			return Baseline{}, 0, 0, err
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
			err = fmt.Errorf("unsupported output type %v at %s", info.Mode(), current)
		}
		if err != nil {
			return Baseline{}, 0, 0, err
		}
		manifest.Entries = append(manifest.Entries, entry)
	}
	manifest = rebaseManifest(manifest, rel, logicalPath)
	return Baseline{Path: logicalPath, Digest: manifest.RootHash()}, logicalBytes, len(paths), nil
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
		return "", fmt.Errorf("output %q changed while it was being inspected", rel)
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
		return "", fmt.Errorf("output %q changed while it was being inspected", rel)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func Collect(
	workspace string,
	jobDir string,
	specs []proto.OutputSpec,
	processSuccess bool,
	maxBytes int64,
) (proto.OutputBundle, bool, error) {
	return CollectContext(context.Background(), workspace, jobDir, specs, processSuccess, maxBytes)
}

// CollectContext collects declared outputs while allowing settlement to be
// canceled by job control or a post-process deadline.
func CollectContext(
	ctx context.Context,
	workspace string,
	jobDir string,
	specs []proto.OutputSpec,
	processSuccess bool,
	maxBytes int64,
) (proto.OutputBundle, bool, error) {
	if err := ctx.Err(); err != nil {
		return proto.OutputBundle{}, false, err
	}
	normalized, err := NormalizeSpecs(specs)
	if err != nil {
		return proto.OutputBundle{}, false, err
	}
	selected := make([]proto.OutputSpec, 0, len(normalized))
	for _, spec := range normalized {
		if spec.Collect == proto.OutputCollectAlways || processSuccess {
			selected = append(selected, spec)
		}
	}
	if len(selected) == 0 {
		return proto.OutputBundle{}, false, nil
	}
	var paths []string
	var preflightBytes int64
	for _, spec := range selected {
		if err := ctx.Err(); err != nil {
			return proto.OutputBundle{}, false, err
		}
		if err := rejectSymlinkParents(workspace, spec.Path); err != nil {
			return proto.OutputBundle{}, false, err
		}
		declared, missing, logicalBytes, err := declaredPathsBoundedContext(ctx, workspace, spec.Path, maxBytes-preflightBytes, MaxOutputEntries-len(paths))
		if err != nil {
			return proto.OutputBundle{}, false, err
		}
		if missing {
			return proto.OutputBundle{}, false, fmt.Errorf("declared output %q does not exist", spec.Path)
		}
		paths = append(paths, declared...)
		preflightBytes += logicalBytes
	}
	manifest, err := snapshot.BuildBoundedContext(ctx, workspace, paths, maxBytes, MaxOutputEntries)
	if err != nil {
		if errors.Is(err, snapshot.ErrLimitExceeded) {
			return proto.OutputBundle{}, false, fmt.Errorf("%w: %v", ErrLimitExceeded, err)
		}
		return proto.OutputBundle{}, false, err
	}
	var size int64
	for _, entry := range manifest.Entries {
		if entry.Type != proto.EntryFile {
			continue
		}
		if maxBytes < 0 || entry.Size > maxBytes-size {
			return proto.OutputBundle{}, false, fmt.Errorf("%w: outputs exceed %d bytes", ErrLimitExceeded, maxBytes)
		}
		size += entry.Size
	}
	selectedPaths := make([]string, len(selected))
	for i, spec := range selected {
		selectedPaths[i] = spec.Path
	}
	bundle := proto.OutputBundle{V: BundleVersion, Paths: selectedPaths, Manifest: manifest, Bytes: size}
	if err := commitBundleContext(ctx, workspace, jobDir, bundle); err != nil {
		return proto.OutputBundle{}, false, err
	}
	return bundle, true, nil
}

func declaredPaths(root, rel string) ([]string, bool, error) {
	paths, missing, _, err := declaredPathsBounded(root, rel, -1, -1)
	return paths, missing, err
}

func declaredPathsBounded(root, rel string, maxBytes int64, maxEntries int) ([]string, bool, int64, error) {
	return declaredPathsBoundedContext(context.Background(), root, rel, maxBytes, maxEntries)
}

func declaredPathsBoundedContext(ctx context.Context, root, rel string, maxBytes int64, maxEntries int) ([]string, bool, int64, error) {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if _, err := os.Lstat(abs); err != nil {
		if os.IsNotExist(err) {
			return nil, true, 0, nil
		}
		return nil, false, 0, err
	}
	var paths []string
	var bytes int64
	err := filepath.WalkDir(abs, func(current string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		workspaceRel, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		workspaceRel = filepath.ToSlash(workspaceRel)
		if pathContainsGitMetadata(workspaceRel) {
			return fmt.Errorf("output path %q targets Git metadata", workspaceRel)
		}
		if maxEntries >= 0 && len(paths) >= maxEntries {
			return fmt.Errorf("%w: outputs exceed %d entries", ErrLimitExceeded, MaxOutputEntries)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			if maxBytes >= 0 && info.Size() > maxBytes-bytes {
				return fmt.Errorf("%w: outputs exceed logical byte limit", ErrLimitExceeded)
			}
			bytes += info.Size()
		}
		paths = append(paths, workspaceRel)
		return nil
	})
	return paths, false, bytes, err
}

func subtreeManifest(manifest proto.Manifest, root string) proto.Manifest {
	filtered := proto.Manifest{Entries: make([]proto.ManifestEntry, 0, len(manifest.Entries))}
	for _, entry := range manifest.Entries {
		if entry.Path == root || strings.HasPrefix(entry.Path, root+"/") {
			filtered.Entries = append(filtered.Entries, entry)
		}
	}
	return filtered
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
			return fmt.Errorf("output path %q passes through symlink %q", rel, current)
		}
		if !info.IsDir() {
			return fmt.Errorf("output path %q passes through non-directory %q", rel, current)
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
			return fmt.Errorf("output path %q passes through symlink %q", rel, current)
		}
		if !info.IsDir() {
			return fmt.Errorf("output path %q passes through non-directory %q", rel, current)
		}
	}
	return nil
}
