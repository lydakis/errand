package outputs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lydakis/errand/internal/archive"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

const (
	BundleVersion           = 1
	BundleDirectory         = "out"
	MaxBundleMetadataBytes  = 64 << 20
	MaxArchiveOverheadBytes = 2*MaxBundleMetadataBytes + (1 << 20)
	bundleFile              = "bundle.json"
	archiveFile             = "archive.tar"
)

var ErrLimitExceeded = errors.New("output byte limit exceeded")

func commitBundle(workspace, jobDir string, bundle proto.OutputBundle) error {
	return commitBundleContext(context.Background(), workspace, jobDir, bundle)
}

func commitBundleContext(ctx context.Context, workspace, jobDir string, bundle proto.OutputBundle) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	metadata, err := marshalBundle(bundle)
	if err != nil {
		return err
	}
	archiveLimit, err := ArchiveByteLimit(bundle.Bytes)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(jobDir, ".outputs-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	archivePath := filepath.Join(tmp, archiveFile)
	f, err := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	limited := &boundedWriter{w: f, remaining: archiveLimit}
	if err := snapshot.PackContext(ctx, limited, workspace, bundle.Manifest); err != nil {
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
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	return syncDirectory(jobDir)
}

// ArchiveByteLimit includes a fixed metadata/header allowance beyond logical
// file bytes, so directory, symlink, tar, and extended-header overhead remains
// bounded independently of content size.
func ArchiveByteLimit(logicalBytes int64) (int64, error) {
	if logicalBytes < 0 || logicalBytes > int64(^uint64(0)>>1)-MaxArchiveOverheadBytes {
		return 0, fmt.Errorf("output archive size overflows")
	}
	return logicalBytes + MaxArchiveOverheadBytes, nil
}

type boundedWriter struct {
	w         io.Writer
	remaining int64
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("%w: output archive exceeds bounded overhead", ErrLimitExceeded)
	}
	n, err := w.w.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func Load(jobDir string) (proto.OutputBundle, error) {
	var bundle proto.OutputBundle
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
		return bundle, fmt.Errorf("output bundle metadata exceeds %d bytes", MaxBundleMetadataBytes)
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return bundle, err
	}
	if err := validateBundle(bundle); err != nil {
		return bundle, err
	}
	return bundle, nil
}

func OpenArchive(jobDir string) (*os.File, error) {
	return os.Open(filepath.Join(jobDir, BundleDirectory, archiveFile))
}

func CleanupTemps(jobDir string) error {
	entries, err := os.ReadDir(jobDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".outputs-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(jobDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func validateBundle(bundle proto.OutputBundle) error {
	if bundle.V != BundleVersion {
		return fmt.Errorf("unsupported output bundle version %d", bundle.V)
	}
	if len(bundle.Manifest.Entries) > MaxOutputEntries {
		return fmt.Errorf("output bundle exceeds %d entries", MaxOutputEntries)
	}
	specs := make([]proto.OutputSpec, len(bundle.Paths))
	for i, outputPath := range bundle.Paths {
		specs[i] = proto.OutputSpec{Path: outputPath}
	}
	normalized, err := NormalizeSpecs(specs)
	if err != nil {
		return err
	}
	for i := range normalized {
		if normalized[i].Path != bundle.Paths[i] {
			return fmt.Errorf("output bundle paths are not sorted")
		}
	}
	if err := archive.Validate(bundle.Manifest); err != nil {
		return err
	}
	if bundle.Bytes < 0 {
		return fmt.Errorf("output bundle has a negative byte count")
	}
	var size int64
	caseFolded := make(map[string]string, len(bundle.Manifest.Entries))
	for i, entry := range bundle.Manifest.Entries {
		if i > 0 && bundle.Manifest.Entries[i-1].Path >= entry.Path {
			return fmt.Errorf("output bundle manifest is not sorted")
		}
		if pathContainsGitMetadata(entry.Path) {
			return fmt.Errorf("output path %q targets Git metadata", entry.Path)
		}
		folded := strings.ToLower(entry.Path)
		if prior, ok := caseFolded[folded]; ok && prior != entry.Path {
			return fmt.Errorf("output paths %q and %q collide on case-insensitive filesystems", prior, entry.Path)
		}
		caseFolded[folded] = entry.Path
		if !bundleEntryDeclared(entry, bundle.Paths) {
			return fmt.Errorf("output bundle contains undeclared path %q", entry.Path)
		}
		if entry.Type == proto.EntryFile {
			if entry.Size > bundle.Bytes-size {
				return fmt.Errorf("output bundle byte count is inconsistent")
			}
			size += entry.Size
		}
	}
	if size != bundle.Bytes {
		return fmt.Errorf("output bundle byte count is %d, want %d", bundle.Bytes, size)
	}
	return nil
}

func bundleEntryDeclared(entry proto.ManifestEntry, outputPaths []string) bool {
	i := sort.SearchStrings(outputPaths, entry.Path)
	if i < len(outputPaths) && outputPaths[i] == entry.Path {
		return true
	}
	if i > 0 && strings.HasPrefix(entry.Path, outputPaths[i-1]+"/") {
		return true
	}
	if entry.Type != proto.EntryDir {
		return false
	}
	prefix := entry.Path + "/"
	i = sort.SearchStrings(outputPaths, prefix)
	return i < len(outputPaths) && strings.HasPrefix(outputPaths[i], prefix)
}

// ValidateBundle verifies that metadata names only declared paths and that its
// manifest and byte accounting are internally consistent.
func ValidateBundle(bundle proto.OutputBundle) error {
	return validateBundle(bundle)
}

// VerifyExtracted re-reads an extracted bundle and refuses local mutation or
// corruption before it is reused or applied.
func VerifyExtracted(root string, bundle proto.OutputBundle) error {
	if err := validateBundle(bundle); err != nil {
		return err
	}
	return snapshot.Pack(io.Discard, root, bundle.Manifest)
}

func Extract(r io.Reader, dest string, bundle proto.OutputBundle, maxBytes int64) error {
	if err := validateBundle(bundle); err != nil {
		return err
	}
	if bundle.Bytes > maxBytes {
		return fmt.Errorf("output bundle exceeds %d bytes", maxBytes)
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("output staging directory is not empty")
	}
	return archive.Extract(r, dest, bundle.Manifest, maxBytes)
}

func writeJSONFile(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeRawJSONFile(path, raw)
}

func marshalBundle(bundle proto.OutputBundle) ([]byte, error) {
	if err := validateBundle(bundle); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(raw)+1 > MaxBundleMetadataBytes {
		return nil, fmt.Errorf("%w: output bundle metadata exceeds %d bytes", ErrLimitExceeded, MaxBundleMetadataBytes)
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
