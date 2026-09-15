package changes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/lydakis/errand/internal/proto"
)

// materializeStage writes the final private base/remote trees directly. Archive
// packaging remains a transport concern, not an intermediate staging format.
type transferMaterializer func(context.Context, string, proto.Manifest) error

func transferDirectorySource(source string) transferMaterializer {
	return func(ctx context.Context, dest string, m proto.Manifest) error {
		return materializeTransferSource(ctx, source, dest, m)
	}
}

func (s *TransferSession) retainedSource() transferMaterializer {
	return func(ctx context.Context, dest string, m proto.Manifest) error {
		return s.Blobs().materializePrivate(ctx, dest, m, s.MaxSourceBytes)
	}
}

func (s *TransferSession) materializeStage(ctx context.Context, source transferMaterializer, dir string, b proto.ChangeBundle) error {
	metadata, err := marshalBundle(b)
	if err != nil {
		return err
	}
	if b.Bytes > s.MaxChangeBytes {
		return ErrByteLimitExceeded
	}
	if err := s.retainedSource()(ctx, filepath.Join(dir, "base"), b.BaseManifest); err != nil {
		return err
	}
	if err := source(ctx, filepath.Join(dir, "remote"), b.RemoteManifest); err != nil {
		return err
	}
	// Persist both tree names and the bundle before the attempt is published.
	if err := writeRawJSONFileWithSync(filepath.Join(dir, bundleFile), metadata, syncStagedData); err != nil {
		return err
	}
	return syncDirectory(dir)
}

// The source is private transfer staging. Temporary access is restored only
// after every copying worker has finished, on success and on failure.
func materializeTransferSource(ctx context.Context, source, dest string, m proto.Manifest) error {
	if err := os.Mkdir(dest, 0700); err != nil {
		return err
	}
	tree, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer tree.Close()
	return materializeSourceTree(ctx, source, tree, m, durableMaterialization(func() error { return syncApplyRootDirectory(tree, ".") }))
}

func materializeSourceTree(ctx context.Context, source string, tree *os.Root, m proto.Manifest, policy materializationPolicy,
) (err error) {
	access, err := makeManifestAccessibleContext(ctx, source, m)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, access.restore()) }()
	return materializeSourceAtRoot(ctx, access.root, tree, m, access.physical, policy)
}

// A nil physical-mode map selects strict source access: no chmod or widening.
func materializeSourceAtRoot(ctx context.Context, source, tree *os.Root, m proto.Manifest, physical map[string]uint32, policy materializationPolicy) (err error) {
	paths := materializationPaths{root: source, verify: true}
	defer func() { err = errors.Join(err, paths.close()) }()
	mode := func(e proto.ManifestEntry) uint32 {
		if physical == nil {
			return e.Mode
		}
		return physical[e.Path]
	}
	for _, e := range m.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		// File metadata is checked against its opened descriptor below. Doing it
		// here as well adds a full metadata pass without strengthening that check.
		if e.Type == proto.EntryFile {
			continue
		}
		if err := checkMaterializationSource(&paths, e, mode(e)); err != nil {
			return err
		}
	}
	return materializeTransferTree(ctx, tree, m, policy, func(e proto.ManifestEntry) (io.ReadCloser, error) {
		parent, name, lease, err := paths.parent(e.Path)
		defer paths.release(lease)
		var info os.FileInfo
		var f *os.File
		if err == nil {
			info, err = parent.Lstat(name)
			if err == nil && !info.Mode().IsRegular() {
				return nil, fmt.Errorf("transfer source %q is not a regular file", e.Path)
			}
			if err == nil {
				f, err = parent.Open(name)
			}
		}
		if errors.Is(err, os.ErrPermission) && !errors.Is(err, errMaterializationParentVerification) {
			f, err = openSearchSourceFile(source, e.Path)
			info = nil // O_NOFOLLOW and the descriptor stat replace the lstat/open pair.
		}
		if err != nil {
			return nil, err
		}
		opened, err := f.Stat()
		if err != nil || !opened.Mode().IsRegular() || (info != nil && !os.SameFile(info, opened)) || opened.Size() != e.Size || uint32(opened.Mode().Perm()) != mode(e) {
			return nil, errors.Join(fmt.Errorf("transfer source %q changed while opening", e.Path), f.Close())
		}
		return &transferSourceReader{File: f, entry: e, mode: mode(e)}, nil
	})
}

type transferSourceReader struct {
	*os.File
	entry proto.ManifestEntry
	mode  uint32
}

func (r *transferSourceReader) Close() error {
	info, err := r.Stat()
	if err == nil && (!info.Mode().IsRegular() || info.Size() != r.entry.Size || uint32(info.Mode().Perm()) != r.mode) {
		err = fmt.Errorf("transfer source %q changed while copying", r.entry.Path)
	}
	return errors.Join(err, r.File.Close())
}

func (r *transferSourceReader) cloneTo(tree *os.Root, name string) (*os.File, error) {
	return cloneFileInto(r.File, tree, name)
}

func checkMaterializationSource(paths *materializationPaths, e proto.ManifestEntry, mode uint32) error {
	parent, name, lease, err := paths.parent(e.Path)
	defer paths.release(lease)
	var info os.FileInfo
	if err == nil {
		info, err = parent.Lstat(name)
	}
	if errors.Is(err, os.ErrPermission) && !errors.Is(err, errMaterializationParentVerification) {
		return checkSearchSource(paths.root, e, mode)
	}
	if err != nil {
		return err
	}
	if uint32(info.Mode().Perm()) != mode {
		return fmt.Errorf("transfer source %q changed mode", e.Path)
	}
	switch e.Type {
	case proto.EntryDir:
		if !info.IsDir() {
			return fmt.Errorf("transfer source %q changed type", e.Path)
		}
	case proto.EntrySymlink:
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("transfer source %q changed type", e.Path)
		}
		target, err := parent.Readlink(name)
		if err != nil {
			return err
		}
		if target != e.Target {
			return fmt.Errorf("transfer source %q changed target", e.Path)
		}
	}
	return nil
}
