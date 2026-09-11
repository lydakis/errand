package changes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/lydakis/errand/internal/archive"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

const (
	workspaceBaseDirectory = "change-base"
	// Bound concurrent disk work and open files per baseline capture.
	baseCaptureWorkers = 16
)

func workspaceBasePath(jobDir string) string {
	return filepath.Join(jobDir, workspaceBaseDirectory)
}

// CaptureWorkspaceBaseContext preserves the submitted tree before the command
// can mutate it. Filesystems with copy-on-write cloning keep this inexpensive;
// other filesystems fall back to verified copies.
func CaptureWorkspaceBaseContext(ctx context.Context, workspace, jobDir string, manifest proto.Manifest) error {
	return captureWorkspaceBaseContext(ctx, workspace, jobDir, manifest, syncCapturedData, syncDirectory)
}

func captureWorkspaceBaseContext(ctx context.Context, workspace, jobDir string, manifest proto.Manifest, syncData func(*os.File) error, syncDir func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := archive.Validate(manifest); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(jobDir, ".change-base-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	directories := make([]proto.ManifestEntry, 0)
	files := make([]proto.ManifestEntry, 0)
	for _, entry := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		src := filepath.Join(workspace, filepath.FromSlash(entry.Path))
		dest := filepath.Join(tmp, filepath.FromSlash(entry.Path))
		switch entry.Type {
		case proto.EntryDir:
			if err := os.MkdirAll(dest, os.FileMode(entry.Mode)|0o700); err != nil {
				return err
			}
			directories = append(directories, entry)
		case proto.EntryFile:
			if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
				return err
			}
			files = append(files, entry)
		case proto.EntrySymlink:
			if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
				return err
			}
			target, err := os.Readlink(src)
			if err != nil {
				return err
			}
			if target != entry.Target {
				return fmt.Errorf("change base %q changed while it was captured", entry.Path)
			}
			if err := os.Symlink(target, dest); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported change base type %q at %s", entry.Type, entry.Path)
		}
	}
	if err := captureBaseFiles(ctx, workspace, tmp, files, syncData); err != nil {
		return err
	}
	physicalModes := make(map[string]uint32, len(directories))
	for _, entry := range directories {
		physicalModes[entry.Path] = uint32((os.FileMode(entry.Mode) | 0o700).Perm())
	}
	if err := snapshot.PackContextWithPhysicalModes(ctx, io.Discard, tmp, manifest, physicalModes); err != nil {
		return fmt.Errorf("verifying captured change base: %w", err)
	}
	if err := finalizeCapturedDirectories(tmp, directories, syncData); err != nil {
		return fmt.Errorf("syncing captured change base: %w", err)
	}
	// Every member lives on this private tree's filesystem. On macOS this
	// full drive flush makes all preceding member fsyncs durable together.
	// Finish it before publication; a second full sync persists the rename.
	if err := syncDir(tmp); err != nil {
		return fmt.Errorf("syncing captured change base: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmp, workspaceBasePath(jobDir)); err != nil {
		return err
	}
	return syncDir(jobDir)
}

func captureBaseFiles(ctx context.Context, workspace, dest string, files []proto.ManifestEntry, syncData func(*os.File) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	queue := make(chan proto.ManifestEntry)
	var workers sync.WaitGroup
	// Overlap disk flushes without creating a goroutine/file descriptor per
	// entry. All files still sync before verification and atomic publication.
	for range min(baseCaptureWorkers, len(files)) {
		workers.Go(func() {
			for entry := range queue {
				err := cloneOrCopyFile(ctx,
					filepath.Join(workspace, filepath.FromSlash(entry.Path)),
					filepath.Join(dest, filepath.FromSlash(entry.Path)), os.FileMode(entry.Mode), syncData)
				if err != nil {
					cancel(err)
					return
				}
			}
		})
	}
send:
	for _, entry := range files {
		select {
		case queue <- entry:
		case <-ctx.Done():
			break send
		}
	}
	close(queue)
	// Even on failure, join every worker before the caller removes the staging
	// directory. No worker may still be writing when cleanup starts.
	workers.Wait()
	return context.Cause(ctx)
}

func cloneOrCopyFile(ctx context.Context, src, dest string, mode fs.FileMode, syncData func(*os.File) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cloneFile(src, dest); err == nil {
		if err := ctx.Err(); err != nil {
			_ = os.Remove(dest)
			return err
		}
		return syncCapturedFile(dest, mode, syncData)
	}
	_ = os.Remove(dest)
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return err
	}
	copyErr := copyContext(ctx, out, in)
	if copyErr != nil {
		closeErr := out.Close()
		_ = os.Remove(dest)
		return errors.Join(copyErr, closeErr)
	}
	chmodErr := out.Chmod(mode.Perm())
	syncErr := syncData(out)
	closeErr := out.Close()
	if err := errors.Join(chmodErr, syncErr, closeErr); err != nil {
		_ = os.Remove(dest)
		return err
	}
	return nil
}

func copyContext(ctx context.Context, dest io.Writer, src io.Reader) error {
	_, err := io.Copy(dest, contextReader{ctx: ctx, reader: src})
	if err != nil {
		return err
	}
	return ctx.Err()
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func syncCapturedFile(path string, mode fs.FileMode, syncData func(*os.File) error) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = errors.Join(file.Chmod(mode.Perm()), syncData(file), file.Close())
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

func finalizeCapturedDirectories(root string, directories []proto.ManifestEntry, syncData func(*os.File) error) error {
	for i := len(directories) - 1; i >= 0; i-- {
		entry := directories[i]
		dir, err := os.Open(filepath.Join(root, filepath.FromSlash(entry.Path)))
		if err != nil {
			return err
		}
		if err := errors.Join(dir.Chmod(os.FileMode(entry.Mode)), syncData(dir), dir.Close()); err != nil {
			return err
		}
	}
	return nil
}
