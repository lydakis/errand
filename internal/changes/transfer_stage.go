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
func materializeTransferSource(ctx context.Context, source, dest string, m proto.Manifest) (err error) {
	access, err := makeManifestAccessibleContext(ctx, source, m)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, access.restore()) }()
	for _, e := range m.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := access.root.Lstat(e.Path)
		if err != nil {
			return err
		}
		if uint32(info.Mode().Perm()) != access.physical[e.Path] {
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
			target, err := access.root.Readlink(e.Path)
			if err != nil {
				return err
			}
			if target != e.Target {
				return fmt.Errorf("transfer source %q changed target", e.Path)
			}
		case proto.EntryFile:
			if !info.Mode().IsRegular() || info.Size() != e.Size {
				return fmt.Errorf("transfer source %q changed type or size", e.Path)
			}
		}
	}
	if err := os.Mkdir(dest, 0700); err != nil {
		return err
	}
	tree, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer tree.Close()
	return materializeTransferTree(ctx, tree, m, func(e proto.ManifestEntry) (io.ReadCloser, error) {
		info, err := access.root.Lstat(e.Path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("transfer source %q is not a regular file", e.Path)
		}
		f, err := access.root.Open(e.Path)
		if err != nil {
			return nil, err
		}
		opened, err := f.Stat()
		if err != nil || !os.SameFile(info, opened) || opened.Size() != e.Size || uint32(opened.Mode().Perm()) != access.physical[e.Path] {
			return nil, errors.Join(fmt.Errorf("transfer source %q changed while opening", e.Path), f.Close())
		}
		return &transferSourceReader{File: f, entry: e, mode: access.physical[e.Path]}, nil
	}, syncStagedData, func() error { return syncApplyRootDirectory(tree, ".") })
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
