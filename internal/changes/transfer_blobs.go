package changes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

const (
	transferBlobTempPrefix = ".blob-"
	// transferBlobUsageName records the published bytes; see Retain.
	transferBlobUsageName = ".usage"
)

// TransferBlobStore retains immutable, hash-addressed source file bodies. Its
// dedicated directory must already exist in private owner-scoped storage, outside
// working trees and independently of evictable upload caches. Callers serialize
// retention, reconstruction and pruning, including across processes. Only Retain
// and Prune change the directory. Retain the
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

// transferBlobUsage records the bytes of published bodies. It is exact whenever
// it exists: Retain removes it before creating insertion files, and the data
// barrier makes that durable before any rename; Prune removes it durably before
// deleting. A new record is flushed with the bodies it counts and named only
// after the final barrier. An interrupted Retain therefore leaves no record, and
// the next Retain scans and reclaims its insertion files. (A system crash on a
// filesystem that reorders directory updates could keep insertion files but not
// the removal; they are never budgeted and wait for Prune.)
type transferBlobUsage struct {
	Bytes *int64 `json:"bytes"`
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
// source. Abandoned insertion files are reclaimed before budgeting new bodies.
// Capacity limits growth; published bodies are never evicted by Retain.
// Budgeting reads the store's usage record and looks up only the needed bodies;
// without a record (first use, or after an interrupted retention or a Prune)
// Retain scans the whole store, reclaims insertion files and records the total.
func (s TransferBlobStore) Retain(ctx context.Context, sourceRoot string, manifest proto.Manifest) error {
	return s.retain(ctx, sourceRoot, manifest, syncStagedData, func(root *os.Root) error { return syncApplyRootDirectory(root, ".") })
}

func (s TransferBlobStore) retain(ctx context.Context, sourceRoot string, manifest proto.Manifest, syncData func(*os.File) error, barrier func(*os.Root) error) (err error) {
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
	files, stored, recorded, err := lookupTransferBlobs(ctx, storage, blobs)
	if err != nil {
		return err
	}
	var missing proto.Manifest
	for hash, e := range blobs {
		if _, exists := files[hash]; exists {
			continue
		}
		missing.Entries = append(missing.Entries, e)
	}
	if len(missing.Entries) > 0 {
		identity, err := applyWorkspaceIdentity(sourceRoot)
		if err != nil {
			return err
		}
		if err := transferStorageOutsideWorkspace(storage.root, identity); err != nil {
			return err
		}
	}
	// The caller's operation lock excludes other writers, so every temporary
	// entry from the initial scan belongs to an interrupted retention. Reclaim
	// it only after validating source/storage separation and before budgeting
	// the same bodies again. Stats and dry-run pruning remain read-only.
	required := stored
	for name, info := range files {
		if !strings.HasPrefix(name, transferBlobTempPrefix) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := storage.verifyPath(); err != nil {
			return err
		}
		current, err := storage.root.Lstat(name)
		if err != nil || !os.SameFile(info, current) {
			return fmt.Errorf("transfer storage changed during temporary cleanup")
		}
		if err := storage.root.Remove(name); err != nil {
			return err
		}
		required -= info.Size()
	}
	for _, e := range missing.Entries {
		if e.Size > 0 && e.Size > s.MaxBytes-required {
			return ErrByteLimitExceeded
		}
		required += e.Size
	}
	var access *treeAccess
	if len(missing.Entries) > 0 {
		access, err = makeManifestAccessibleContext(ctx, sourceRoot, missing)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, access.restore()) }()
		if err := transferStorageOutsideWorkspace(storage.root, access.rootIdentity); err != nil {
			return err
		}
	}
	hashes := make([]string, 0, len(blobs))
	for hash := range blobs {
		hashes = append(hashes, hash)
	}
	prepared := make([]string, len(hashes))
	defer func() {
		for _, name := range prepared {
			if name != "" {
				_ = storage.root.Remove(name)
			}
		}
	}()
	if recorded && len(missing.Entries) > 0 {
		// Drop the record before creating any insertion file. An interruption
		// from here on leaves no record, so the next Retain scans and reclaims;
		// the data barrier makes the removal durable before any rename.
		if err := storage.root.Remove(transferBlobUsageName); err != nil {
			return err
		}
	}
	if err := runStagingTasksContext(ctx, len(hashes), func(ctx context.Context, i int) error {
		hash := hashes[i]
		e := blobs[hash]
		if _, exists := files[hash]; exists {
			in, err := openTransferBlob(storage.root, hash, e)
			if err != nil {
				return err
			}
			err = copyTransferBlob(ctx, io.Discard, in, e)
			return errors.Join(err, in.Close())
		}
		in, err := openTransferBlob(access.root, e.Path, e)
		if err != nil {
			return err
		}
		prepared[i], err = prepareTransferBlob(ctx, storage, in, e, syncData)
		return errors.Join(err, in.Close())
	}); err != nil {
		return err
	}
	usage := ""
	if !recorded || len(missing.Entries) > 0 {
		// Flush the new record with the bodies; it is named after the final barrier.
		if usage, err = prepareTransferBlobUsage(ctx, storage, required, syncData); err != nil {
			return err
		}
		defer func() {
			if usage != "" {
				_ = storage.root.Remove(usage)
			}
		}()
	}

	if len(missing.Entries) > 0 {
		// Until this full flush completes, bodies have only temporary names.
		// A surviving hash name must never refer to unflushed device-cache data,
		// even if the process stops before the final directory sync/checkpoint.
		if err := errors.Join(barrier(storage.root), storage.verifyPath()); err != nil {
			return err
		}
		dir, err := storage.root.Open(".")
		if err != nil {
			return err
		}
		defer dir.Close()
		for i, name := range prepared {
			if name == "" {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := storage.verifyPath(); err != nil {
				return err
			}
			if err := renameNoReplace(dir, name, dir, hashes[i]); err != nil {
				return err
			}
			prepared[i] = ""
		}
	}
	// Also complete directory publication when a retry finds all bodies stored.
	if err := errors.Join(barrier(storage.root), storage.verifyPath()); err != nil {
		return err
	}
	if usage != "" {
		// Unsynced: a crash that loses this name only costs the next Retain a scan.
		if err := storage.root.Rename(usage, transferBlobUsageName); err != nil {
			return err
		}
		usage = ""
	}
	return nil
}

// lookupTransferBlobs returns the needed bodies already stored and the stored
// bytes. With a usage record it lstats only the needed names. Otherwise it
// scans, and the files and bytes also include abandoned insertion files.
func lookupTransferBlobs(ctx context.Context, storage *applyDestination, blobs map[string]proto.ManifestEntry) (map[string]os.FileInfo, int64, bool, error) {
	stored, recorded, err := readTransferBlobUsage(storage)
	if err != nil {
		return nil, 0, false, err
	}
	if !recorded {
		files, stats, err := scanTransferBlobs(ctx, storage)
		return files, stats.Bytes, false, err
	}
	files := make(map[string]os.FileInfo)
	for hash := range blobs {
		if err := ctx.Err(); err != nil {
			return nil, 0, false, err
		}
		info, err := storage.root.Lstat(hash)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, 0, false, err
		}
		if !info.Mode().IsRegular() {
			return nil, 0, false, fmt.Errorf("transfer storage entry %q is not a regular file", hash)
		}
		files[hash] = info
	}
	return files, stored, true, storage.verifyPath()
}

// readTransferBlobUsage treats a missing or malformed record as absent, so the
// caller rebuilds it from a scan.
func readTransferBlobUsage(storage *applyDestination) (int64, bool, error) {
	info, err := storage.root.Lstat(transferBlobUsageName)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !info.Mode().IsRegular() {
		return 0, false, fmt.Errorf("transfer storage entry %q is not a regular file", transferBlobUsageName)
	}
	f, err := storage.root.Open(transferBlobUsageName)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return 0, false, fmt.Errorf("transfer storage entry %q changed while opening", transferBlobUsageName)
	}
	raw, err := io.ReadAll(io.LimitReader(f, 1024))
	if err != nil {
		return 0, false, err
	}
	var usage transferBlobUsage
	if json.Unmarshal(raw, &usage) != nil || usage.Bytes == nil || *usage.Bytes < 0 {
		return 0, false, nil
	}
	return *usage.Bytes, true, nil
}

// prepareTransferBlobUsage returns an unpublished, member-synced record under
// an insertion name, so an abandoned one is reclaimed like any other.
func prepareTransferBlobUsage(ctx context.Context, storage *applyDestination, stored int64, syncData func(*os.File) error) (string, error) {
	raw, err := json.Marshal(transferBlobUsage{Bytes: &stored})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	entry := proto.ManifestEntry{Path: transferBlobUsageName, Type: proto.EntryFile, Size: int64(len(raw)), SHA256: hex.EncodeToString(sum[:])}
	return prepareTransferBlob(ctx, storage, bytes.NewReader(raw), entry, syncData)
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

// prepareTransferBlob returns a verified, member-synced temporary body. The
// caller owns cleanup and must complete the full batch barrier before renaming.
func prepareTransferBlob(ctx context.Context, storage *applyDestination, in io.Reader, entry proto.ManifestEntry, syncData func(*os.File) error) (name string, err error) {
	tmp := transferBlobTempPrefix + proto.NewULID()
	out, err := storage.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = storage.root.Remove(tmp)
		}
	}()
	copyErr := copyTransferBlob(ctx, out, in, entry)
	if err := errors.Join(copyErr, syncData(out), out.Close()); err != nil {
		return "", err
	}
	if err := storage.verifyPath(); err != nil {
		return "", err
	}
	return tmp, nil
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
			if !isTemp && name != transferBlobUsageName {
				_, err := hex.DecodeString(name)
				if err != nil || len(name) != 64 || name != strings.ToLower(name) {
					return nil, stats, fmt.Errorf("unexpected transfer storage entry %q", name)
				}
			}
			if !info.Mode().IsRegular() {
				return nil, stats, fmt.Errorf("transfer storage entry %q is not a regular file", name)
			}
			if name == transferBlobUsageName {
				continue
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
// reference and directory validation without modifying storage. Otherwise the
// usage record is durably dropped before any removal; the next Retain rebuilds it.
// Pin validation checks manifest shape, presence and size, not content hashes;
// Retain and MaterializeBase verify contents when reusing bodies.
func (s TransferBlobStore) Prune(ctx context.Context, keep []proto.Manifest, dryRun bool) (TransferBlobPruneResult, error) {
	return s.prune(ctx, keep, dryRun, func(root *os.Root) error { return syncApplyRootDirectory(root, ".") })
}

func (s TransferBlobStore) prune(ctx context.Context, keep []proto.Manifest, dryRun bool, barrier func(*os.Root) error) (TransferBlobPruneResult, error) {
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
	if !dryRun {
		if err := storage.verifyPath(); err != nil {
			return result, err
		}
		if err := storage.root.Remove(transferBlobUsageName); err == nil {
			if err := barrier(storage.root); err != nil {
				return result, err
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return result, err
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
		if err := barrier(storage.root); err != nil {
			return result, err
		}
	}
	return result, storage.verifyPath()
}
