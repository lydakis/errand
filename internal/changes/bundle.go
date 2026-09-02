package changes

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lydakis/errand/internal/archive"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

const (
	BundleVersion           = 1
	BundleDirectory         = "changes"
	MaxBundleMetadataBytes  = 64 << 20
	MaxArchiveOverheadBytes = 2*MaxBundleMetadataBytes + (1 << 20)
	bundleFile              = "bundle.json"
	baseArchiveFile         = "base.tar"
	remoteArchiveFile       = "remote.tar"
)

var (
	ErrLimitExceeded      = errors.New("change limit exceeded")
	ErrByteLimitExceeded  = fmt.Errorf("%w: byte limit exceeded", ErrLimitExceeded)
	ErrEntryLimitExceeded = fmt.Errorf("%w: entry limit exceeded", ErrLimitExceeded)
)

func commitBundle(baseRoot, remoteRoot, jobDir string, bundle proto.ChangeBundle) error {
	return commitBundleContext(context.Background(), baseRoot, remoteRoot, jobDir, bundle)
}

func commitBundleContext(ctx context.Context, baseRoot, remoteRoot, jobDir string, bundle proto.ChangeBundle) error {
	return commitBundleWithPhysicalModesContext(ctx, baseRoot, remoteRoot, jobDir, bundle, nil, nil)
}

func commitBundleWithPhysicalModesContext(
	ctx context.Context,
	baseRoot, remoteRoot, jobDir string,
	bundle proto.ChangeBundle,
	basePhysical, remotePhysical map[string]uint32,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	metadata, err := marshalBundle(bundle)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(jobDir, ".changes-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	var baseAccess *treeAccess
	if len(bundle.BaseManifest.Entries) != 0 {
		baseAccess, err = makeManifestAccessibleContext(ctx, baseRoot, bundle.BaseManifest)
		if err != nil {
			return err
		}
		if basePhysical == nil {
			basePhysical = baseAccess.physical
		}
	}
	basePackErr := packBundleArchive(ctx, tmp, baseArchiveFile, baseRoot, bundle.BaseManifest, basePhysical)
	var baseRestoreErr error
	if baseAccess != nil {
		baseRestoreErr = baseAccess.restore()
	}
	if err := errors.Join(basePackErr, baseRestoreErr); err != nil {
		return err
	}
	if err := packBundleArchive(ctx, tmp, remoteArchiveFile, remoteRoot, bundle.RemoteManifest, remotePhysical); err != nil {
		return err
	}
	if err := writeRawJSONFile(filepath.Join(tmp, bundleFile), metadata); err != nil {
		return err
	}
	if err := syncDirectory(tmp); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dest := filepath.Join(jobDir, BundleDirectory)
	return publishBundleDirectory(tmp, dest, jobDir, syncDirectory)
}

func publishBundleDirectory(tmp, dest, parent string, syncParent func(string) error) error {
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	if err := syncParent(parent); err != nil {
		cleanupErr := RemoveTree(dest)
		resyncErr := syncParent(parent)
		return errors.Join(err, cleanupErr, resyncErr)
	}
	return nil
}

func packBundleArchive(ctx context.Context, dir, name, root string, manifest proto.Manifest, physicalModes map[string]uint32) error {
	archiveLimit, err := ArchiveByteLimit(manifestBytes(manifest))
	if err != nil {
		return err
	}
	archivePath := filepath.Join(dir, name)
	f, err := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	limited := &boundedWriter{w: f, remaining: archiveLimit}
	if err := snapshot.PackContextWithPhysicalModes(ctx, limited, root, manifest, physicalModes); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return nil
}

// ArchiveByteLimit includes a fixed metadata/header allowance beyond logical
// file bytes, so directory, symlink, tar, and extended-header overhead remains
// bounded independently of content size.
func ArchiveByteLimit(logicalBytes int64) (int64, error) {
	if logicalBytes < 0 || logicalBytes > math.MaxInt64-MaxArchiveOverheadBytes {
		return 0, fmt.Errorf("%w: change archive size overflows", ErrByteLimitExceeded)
	}
	return logicalBytes + MaxArchiveOverheadBytes, nil
}

type boundedWriter struct {
	w         io.Writer
	remaining int64
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("%w: change archive exceeds bounded overhead", ErrByteLimitExceeded)
	}
	n, err := w.w.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func Load(jobDir string) (proto.ChangeBundle, error) {
	var bundle proto.ChangeBundle
	f, err := os.Open(filepath.Join(jobDir, BundleDirectory, bundleFile))
	if err != nil {
		return bundle, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxBundleMetadataBytes+1))
	if err != nil {
		return bundle, err
	}
	if len(raw) > MaxBundleMetadataBytes {
		return bundle, fmt.Errorf("change bundle metadata exceeds %d bytes", MaxBundleMetadataBytes)
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return bundle, err
	}
	if err := validateBundle(bundle); err != nil {
		return bundle, err
	}
	return bundle, nil
}

func OpenBaseArchive(jobDir string) (*os.File, error) {
	return os.Open(filepath.Join(jobDir, BundleDirectory, baseArchiveFile))
}

func OpenRemoteArchive(jobDir string) (*os.File, error) {
	return os.Open(filepath.Join(jobDir, BundleDirectory, remoteArchiveFile))
}

func CleanupTemps(jobDir string) error {
	entries, err := os.ReadDir(jobDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() ||
			(!strings.HasPrefix(entry.Name(), ".changes-") && !strings.HasPrefix(entry.Name(), ".change-base-")) {
			continue
		}
		if err := RemoveTree(filepath.Join(jobDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func validateBundle(bundle proto.ChangeBundle) error {
	if bundle.V != BundleVersion {
		return fmt.Errorf("unsupported change bundle version %d", bundle.V)
	}
	if len(bundle.BaselineRoot) != 64 {
		return fmt.Errorf("change bundle has an invalid baseline root")
	}
	if _, err := hex.DecodeString(bundle.BaselineRoot); err != nil {
		return fmt.Errorf("change bundle has an invalid baseline root")
	}
	if len(bundle.BaseManifest.Entries) > MaxChangeEntries || len(bundle.RemoteManifest.Entries) > MaxChangeEntries {
		return fmt.Errorf("%w: change bundle exceeds %d entries per tree", ErrEntryLimitExceeded, MaxChangeEntries)
	}
	rootSet := make(map[string]struct{}, len(bundle.Paths))
	caseFoldedRoots := make(map[string]string, len(bundle.Paths))
	metadataSet := make(map[string]struct{}, len(bundle.MetadataPaths))
	for i, metadataPath := range bundle.MetadataPaths {
		if i > 0 && bundle.MetadataPaths[i-1] >= metadataPath {
			return fmt.Errorf("change bundle metadata paths are not sorted")
		}
		if index := sort.SearchStrings(bundle.Paths, metadataPath); index >= len(bundle.Paths) || bundle.Paths[index] != metadataPath {
			return fmt.Errorf("change bundle metadata path %q is not a change root", metadataPath)
		}
		metadataSet[metadataPath] = struct{}{}
	}
	for i, changePath := range bundle.Paths {
		if err := validatePath(changePath); err != nil {
			return err
		}
		if pathUsesApplyTransaction(changePath) {
			return fmt.Errorf("change path %q uses Errand's reserved apply namespace", changePath)
		}
		if i > 0 && bundle.Paths[i-1] >= changePath {
			return fmt.Errorf("change bundle paths are not sorted")
		}
		for parent := path.Dir(changePath); parent != "."; parent = path.Dir(parent) {
			if _, ok := rootSet[parent]; ok {
				if _, metadata := metadataSet[parent]; !metadata {
					return fmt.Errorf("change paths %q and %q overlap", parent, changePath)
				}
			}
			if prior, ok := caseFoldedRoots[strings.ToLower(parent)]; ok {
				if _, metadata := metadataSet[prior]; prior != parent || !metadata {
					return fmt.Errorf("change paths %q and %q overlap on case-insensitive filesystems", prior, changePath)
				}
			}
		}
		folded := strings.ToLower(changePath)
		if prior, ok := caseFoldedRoots[folded]; ok {
			return fmt.Errorf("change paths %q and %q collide on case-insensitive filesystems", prior, changePath)
		}
		rootSet[changePath] = struct{}{}
		caseFoldedRoots[folded] = changePath
		base := subtreeManifest(bundle.BaseManifest, changePath)
		remote := subtreeManifest(bundle.RemoteManifest, changePath)
		if _, metadata := metadataSet[changePath]; metadata {
			base = exactManifestEntry(bundle.BaseManifest, changePath)
			remote = exactManifestEntry(bundle.RemoteManifest, changePath)
		}
		if len(base.Entries) == 0 && len(remote.Entries) == 0 {
			return fmt.Errorf("change bundle has no value for %q", changePath)
		}
		if base.RootHash() == remote.RootHash() {
			return fmt.Errorf("change bundle path %q is unchanged", changePath)
		}
		_, metadata := metadataSet[changePath]
		compactModeChange := len(base.Entries) == 1 && len(remote.Entries) == 1 &&
			base.Entries[0].Path == changePath && remote.Entries[0].Path == changePath &&
			base.Entries[0].Type == proto.EntryDir && remote.Entries[0].Type == proto.EntryDir
		if metadata != compactModeChange {
			return fmt.Errorf("change bundle metadata classification for %q is inconsistent", changePath)
		}
	}
	if bundle.Bytes < 0 {
		return fmt.Errorf("change bundle has a negative byte count")
	}
	baseBytes, err := validateBundleManifest(bundle.BaseManifest, bundle.Paths, "base")
	if err != nil {
		return err
	}
	remoteBytes, err := validateBundleManifest(bundle.RemoteManifest, bundle.Paths, "remote")
	if err != nil {
		return err
	}
	if baseBytes > bundle.Bytes || remoteBytes > bundle.Bytes-baseBytes || baseBytes+remoteBytes != bundle.Bytes {
		return fmt.Errorf("change bundle byte count is inconsistent")
	}
	return nil
}

func exactManifestEntry(manifest proto.Manifest, entryPath string) proto.Manifest {
	index := sort.Search(len(manifest.Entries), func(i int) bool {
		return manifest.Entries[i].Path >= entryPath
	})
	if index == len(manifest.Entries) || manifest.Entries[index].Path != entryPath {
		return proto.Manifest{}
	}
	return proto.Manifest{Entries: []proto.ManifestEntry{manifest.Entries[index]}}
}

func validateBundleManifest(manifest proto.Manifest, paths []string, label string) (int64, error) {
	if err := archive.Validate(manifest); err != nil {
		return 0, err
	}
	var size int64
	caseFolded := make(map[string]string, len(manifest.Entries))
	for i, entry := range manifest.Entries {
		if i > 0 && manifest.Entries[i-1].Path >= entry.Path {
			return 0, fmt.Errorf("change bundle %s manifest is not sorted", label)
		}
		if pathContainsGitMetadata(entry.Path) {
			return 0, fmt.Errorf("change path %q targets Git metadata", entry.Path)
		}
		if pathUsesApplyTransaction(entry.Path) {
			return 0, fmt.Errorf("change path %q uses Errand's reserved apply namespace", entry.Path)
		}
		folded := strings.ToLower(entry.Path)
		if prior, ok := caseFolded[folded]; ok && prior != entry.Path {
			return 0, fmt.Errorf("change paths %q and %q collide on case-insensitive filesystems", prior, entry.Path)
		}
		caseFolded[folded] = entry.Path
		if !bundleEntryDeclared(entry, paths) {
			return 0, fmt.Errorf("change bundle contains a path outside its change roots: %q", entry.Path)
		}
		if entry.Type == proto.EntryFile {
			if entry.Size > math.MaxInt64-size {
				return 0, fmt.Errorf("change bundle byte count overflows")
			}
			size += entry.Size
		}
	}
	return size, nil
}

func manifestBytes(manifest proto.Manifest) int64 {
	var size int64
	for _, entry := range manifest.Entries {
		if entry.Type == proto.EntryFile {
			size += entry.Size
		}
	}
	return size
}

func bundleEntryDeclared(entry proto.ManifestEntry, changePaths []string) bool {
	i := sort.SearchStrings(changePaths, entry.Path)
	if i < len(changePaths) && changePaths[i] == entry.Path {
		return true
	}
	for parent := path.Dir(entry.Path); parent != "."; parent = path.Dir(parent) {
		i = sort.SearchStrings(changePaths, parent)
		if i < len(changePaths) && changePaths[i] == parent {
			return true
		}
	}
	if entry.Type != proto.EntryDir {
		return false
	}
	prefix := entry.Path + "/"
	i = sort.SearchStrings(changePaths, prefix)
	return i < len(changePaths) && strings.HasPrefix(changePaths[i], prefix)
}

// ValidateBundle verifies that metadata names only declared paths and that its
// manifest and byte accounting are internally consistent.
func ValidateBundle(bundle proto.ChangeBundle) error {
	return validateBundle(bundle)
}

// VerifyExtracted re-reads an extracted bundle and refuses local mutation or
// corruption before it is reused or applied.
func VerifyExtracted(root string, bundle proto.ChangeBundle) error {
	if err := validateBundle(bundle); err != nil {
		return err
	}
	return errors.Join(
		verifyExtractedTree(filepath.Join(root, "base"), bundle.BaseManifest),
		verifyExtractedTree(filepath.Join(root, "remote"), bundle.RemoteManifest),
	)
}

func verifyExtractedTree(root string, manifest proto.Manifest) error {
	access, err := makeTreeAccessible(root)
	if err != nil {
		return err
	}
	packErr := snapshot.PackContextWithPhysicalModes(
		context.Background(), io.Discard, root, manifest, access.physical,
	)
	return errors.Join(packErr, access.restore())
}

func ExtractBase(r io.Reader, dest string, bundle proto.ChangeBundle, maxBytes int64) error {
	if err := validateBundle(bundle); err != nil {
		return err
	}
	if bundle.Bytes > maxBytes {
		return fmt.Errorf("change bundle exceeds %d bytes", maxBytes)
	}
	return extractTree(r, dest, bundle.BaseManifest, maxBytes)
}

func ExtractRemote(r io.Reader, dest string, bundle proto.ChangeBundle, maxBytes int64) error {
	if err := validateBundle(bundle); err != nil {
		return err
	}
	if bundle.Bytes > maxBytes {
		return fmt.Errorf("change bundle exceeds %d bytes", maxBytes)
	}
	return extractTree(r, dest, bundle.RemoteManifest, maxBytes)
}

func extractTree(r io.Reader, dest string, manifest proto.Manifest, maxBytes int64) error {
	entries, err := os.ReadDir(dest)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("change staging directory is not empty")
	}
	return archive.Extract(r, dest, manifest, maxBytes)
}

func writeJSONFile(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeRawJSONFile(path, raw)
}

func marshalBundle(bundle proto.ChangeBundle) ([]byte, error) {
	if err := validateBundle(bundle); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(raw)+1 > MaxBundleMetadataBytes {
		return nil, fmt.Errorf("%w: change bundle metadata exceeds %d bytes", ErrByteLimitExceeded, MaxBundleMetadataBytes)
	}
	return raw, nil
}

func writeRawJSONFile(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
