package changes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

const transferBlobTempPrefix = ".blob-"

// TransferBlobStore retains immutable, hash-addressed source file bodies. Its
// dedicated directory must already exist in private owner-scoped storage, outside
// working trees and independently of evictable upload caches. Callers serialize
// retention, reconstruction and pruning, including across processes. Retain the
// source bodies before publishing a checkpoint that references them. Interrupted
// retention may leave verified, unreferenced blobs; Prune can reclaim them.
type TransferBlobStore struct {
	Directory string
	MaxBytes  int64
}

type TransferBlobStats struct {
	Blobs int
	// Bytes includes abandoned insertion files; Blobs counts published bodies only.
	Bytes int64
}

type TransferBlobPruneResult struct {
	RemovedBlobs int
	FreedBytes   int64
}

func (s TransferBlobStore) open() (*applyDestination, error) {
	if !filepath.IsAbs(s.Directory) || s.MaxBytes < 0 {
		return nil, fmt.Errorf("transfer blob storage requires an absolute directory and nonnegative capacity")
	}
	return openApplyDestination(s.Directory)
}

func transferBlobEntries(manifest proto.Manifest) (map[string]proto.ManifestEntry, error) {
	if err := validateCheckpointManifest(manifest); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxBundleMetadataBytes {
		return nil, fmt.Errorf("transfer manifest exceeds size limit")
	}
	blobs := make(map[string]proto.ManifestEntry)
	for _, e := range manifest.Entries {
		if e.Type != proto.EntryFile {
			continue
		}
		if e.Size == math.MaxInt64 {
			return nil, ErrByteLimitExceeded
		}
		hash := strings.ToLower(e.SHA256)
		if prior, ok := blobs[hash]; ok && prior.Size != e.Size {
			return nil, fmt.Errorf("inconsistent transfer blob size")
		}
		blobs[hash] = e
	}
	return blobs, nil
}

// Retain copies and verifies each distinct body before atomic publication. A
// cached body is verified too; corruption is an error, never an implicit miss.
// Only missing bodies require sourceRoot. When needed, sourceRoot must be a
// caller-owned stable staging tree, not a live workspace: access may temporarily
// widen permissions, restoring them on return but not after a process crash.
// Callers must keep that staging tree unchanged throughout the operation.
// Already-stored bodies remain reusable after lowering MaxBytes or losing the
// source. Capacity limits additional bytes, including abandoned insertion files.
func (s TransferBlobStore) Retain(ctx context.Context, sourceRoot string, manifest proto.Manifest) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	blobs, err := transferBlobEntries(manifest)
	if err != nil {
		return err
	}
	storage, err := s.open()
	if err != nil {
		return err
	}
	defer storage.Close()
	files, stats, err := scanTransferBlobs(ctx, storage)
	if err != nil {
		return err
	}
	required := stats.Bytes
	var missing proto.Manifest
	for hash, e := range blobs {
		if _, exists := files[hash]; exists {
			continue
		}
		if e.Size > 0 && e.Size > s.MaxBytes-required {
			return ErrByteLimitExceeded
		}
		required += e.Size
		missing.Entries = append(missing.Entries, e)
	}
	var access *treeAccess
	if len(missing.Entries) > 0 {
		identity, err := applyWorkspaceIdentity(sourceRoot)
		if err != nil {
			return err
		}
		if err := transferStorageOutsideWorkspace(storage.root, identity); err != nil {
			return err
		}
		access, err = makeManifestAccessibleContext(ctx, sourceRoot, missing)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, access.restore()) }()
		if err := transferStorageOutsideWorkspace(storage.root, access.rootIdentity); err != nil {
			return err
		}
	}
	for hash, e := range blobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, exists := files[hash]; exists {
			in, err := openTransferBlob(storage.root, hash, e)
			if err != nil {
				return err
			}
			err = copyTransferBlob(ctx, io.Discard, in, e)
			if err := errors.Join(err, in.Close()); err != nil {
				return err
			}
			continue
		}
		in, err := openTransferBlob(access.root, e.Path, e)
		if err != nil {
			return err
		}
		err = retainTransferBlob(ctx, storage, in, hash, e)
		if err := errors.Join(err, in.Close()); err != nil {
			return err
		}
	}
	// A previous process may have stopped after renaming a body but before
	// syncing its directory entry. Complete publication even for cached retries.
	return errors.Join(syncApplyRootDirectory(storage.root, "."), storage.verifyPath())
}

func openTransferBlob(root *os.Root, name string, entry proto.ManifestEntry) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != entry.Size {
		return nil, fmt.Errorf("transfer content %q is not the expected regular file", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		f.Close()
		return nil, fmt.Errorf("transfer content %q changed while opening", name)
	}
	return f, nil
}

func copyTransferBlob(ctx context.Context, out io.Writer, in io.Reader, entry proto.ManifestEntry) error {
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, hash), io.LimitReader(contextReader{ctx: ctx, reader: in}, entry.Size+1))
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if n != entry.Size || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), entry.SHA256) {
		return fmt.Errorf("transfer content %q does not match its recorded hash and size", entry.Path)
	}
	return nil
}

func retainTransferBlob(ctx context.Context, storage *applyDestination, in io.Reader, hash string, entry proto.ManifestEntry) error {
	tmp := transferBlobTempPrefix + proto.NewULID()
	out, err := storage.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer storage.root.Remove(tmp)
	copyErr := copyTransferBlob(ctx, out, in, entry)
	if err := errors.Join(copyErr, out.Sync(), out.Close()); err != nil {
		return err
	}
	if err := storage.verifyPath(); err != nil {
		return err
	}
	dir, err := storage.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := renameNoReplace(dir, tmp, dir, hash); err != nil {
		return err
	}
	return errors.Join(dir.Sync(), storage.verifyPath())
}

func (s TransferBlobStore) Stats(ctx context.Context) (TransferBlobStats, error) {
	storage, err := s.open()
	if err != nil {
		return TransferBlobStats{}, err
	}
	defer storage.Close()
	_, stats, err := scanTransferBlobs(ctx, storage)
	return stats, err
}

func scanTransferBlobs(ctx context.Context, storage *applyDestination) (map[string]os.FileInfo, TransferBlobStats, error) {
	files := make(map[string]os.FileInfo)
	var stats TransferBlobStats
	dir, err := storage.root.Open(".")
	if err != nil {
		return nil, stats, err
	}
	defer dir.Close()
	for {
		if err := ctx.Err(); err != nil {
			return nil, stats, err
		}
		entries, err := dir.ReadDir(256)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, stats, err
		}
		for _, entry := range entries {
			name := entry.Name()
			info, err := storage.root.Lstat(name)
			if err != nil {
				return nil, stats, err
			}
			isTemp := strings.HasPrefix(name, transferBlobTempPrefix)
			if !isTemp {
				_, err := hex.DecodeString(name)
				if err != nil || len(name) != 64 || name != strings.ToLower(name) {
					return nil, stats, fmt.Errorf("unexpected transfer storage entry %q", name)
				}
			}
			if !info.Mode().IsRegular() {
				return nil, stats, fmt.Errorf("transfer storage entry %q is not a regular file", name)
			}
			if info.Size() > math.MaxInt64-stats.Bytes {
				return nil, stats, ErrByteLimitExceeded
			}
			stats.Bytes += info.Size()
			if !isTemp {
				stats.Blobs++
			}
			files[name] = info
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	return files, stats, storage.verifyPath()
}

// Prune removes only unreferenced content. keep must include all checkpoint,
// staged and in-flight manifests in this store's scope. Callers hold the same
// operation lock used by Retain/MaterializeBase while assembling keep and pruning.
// No TTL or size eviction can discard pinned data. Dry runs perform the same
// reference and directory validation without modifying storage.
// Pin validation checks manifest shape, presence and size, not content hashes;
// Retain and MaterializeBase verify contents when reusing bodies.
func (s TransferBlobStore) Prune(ctx context.Context, keep []proto.Manifest, dryRun bool) (TransferBlobPruneResult, error) {
	var result TransferBlobPruneResult
	pinned := make(map[string]proto.ManifestEntry)
	for _, manifest := range keep {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		blobs, err := transferBlobEntries(manifest)
		if err != nil {
			return result, err
		}
		for hash, e := range blobs {
			if prior, ok := pinned[hash]; ok && prior.Size != e.Size {
				return result, fmt.Errorf("inconsistent pinned transfer blob size")
			}
			pinned[hash] = e
		}
	}
	storage, err := s.open()
	if err != nil {
		return result, err
	}
	defer storage.Close()
	files, _, err := scanTransferBlobs(ctx, storage)
	if err != nil {
		return result, err
	}
	for hash, e := range pinned {
		if info, ok := files[hash]; !ok || info.Size() != e.Size {
			return result, fmt.Errorf("pinned transfer blob %s is missing or has changed size", hash)
		}
	}
	for name, info := range files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if _, keep := pinned[name]; keep {
			continue
		}
		if !dryRun {
			if err := storage.verifyPath(); err != nil {
				return result, err
			}
			current, err := storage.root.Lstat(name)
			if err != nil || !os.SameFile(info, current) {
				return result, fmt.Errorf("transfer storage changed during pruning")
			}
			if err := storage.root.Remove(name); err != nil {
				return result, err
			}
		}
		result.FreedBytes += info.Size()
		if !strings.HasPrefix(name, transferBlobTempPrefix) {
			result.RemovedBlobs++
		}
	}
	if !dryRun {
		if err := syncApplyRootDirectory(storage.root, "."); err != nil {
			return result, err
		}
	}
	return result, storage.verifyPath()
}
