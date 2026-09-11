package namedcache

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

// A linked tree compares directory entries and inode identities. Shared inode
// bytes/modes already propagate through native links; they must never cause an
// unchanged directory layout to supersede a newer installation.
func TreeFingerprint(ctx context.Context, workspace, path string, baseline ...string) (string, error) {
	linked := len(baseline) != 0 && strings.HasPrefix(baseline[0], "links:")
	root, err := treeWorkspace(workspace, path, false)
	if err != nil {
		return "", err
	}
	defer root.Close()
	info, err := root.Lstat(path)
	if os.IsNotExist(err) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("installed cache %q must remain a real directory", path)
	}
	h := sha256.New()
	treePath := filepath.Join(workspace, path)
	err = filepath.WalkDir(treePath, func(full string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		name, err := filepath.Rel(treePath, full)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		target := ""
		if info.Mode()&os.ModeSymlink != 0 {
			target, err = os.Readlink(full)
			if err != nil {
				return err
			}
		}
		return hashTreeEntry(h, name, info, target, linked)
	})
	return treeDigest(h, linked), err
}

func ValidTreeBaseline(base string) bool {
	if base == "absent" {
		return true
	}
	value := strings.TrimPrefix(base, "private:")
	if strings.HasPrefix(base, "links:") {
		value = strings.TrimPrefix(base, "links:")
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func treeDigest(h hash.Hash, linked bool) string {
	prefix := "private:"
	if linked {
		prefix = "links:"
	}
	return prefix + hex.EncodeToString(h.Sum(nil))
}

func hashTreeEntry(h hash.Hash, name string, info fs.FileInfo, target string, linked bool) error {
	var numbers [32]byte
	mode := info.Mode()
	// Symlink permissions are not portable and are not used to resolve links.
	if mode&os.ModeSymlink != 0 {
		mode = os.ModeSymlink
	}
	if linked && mode.IsRegular() {
		mode = 0
	}
	binary.LittleEndian.PutUint64(numbers[:8], uint64(len(name)))
	binary.LittleEndian.PutUint64(numbers[8:16], uint64(mode))
	h.Write(numbers[:16])
	io.WriteString(h, name)
	switch {
	case info.IsDir():
	case info.Mode()&os.ModeSymlink != 0:
		binary.LittleEndian.PutUint64(numbers[:8], uint64(len(target)))
		h.Write(numbers[:8])
		io.WriteString(h, target)
	case info.Mode().IsRegular():
		if linked {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return fmt.Errorf("file identity unavailable: %s", name)
			}
			binary.LittleEndian.PutUint64(numbers[:8], uint64(stat.Dev))
			binary.LittleEndian.PutUint64(numbers[8:16], uint64(stat.Ino))
		} else {
			binary.LittleEndian.PutUint64(numbers[:8], uint64(info.Size()))
			binary.LittleEndian.PutUint64(numbers[8:16], uint64(info.ModTime().UnixNano()))
		}
		h.Write(numbers[:16])
	default:
		return fmt.Errorf("unsupported installed cache entry %q", name)
	}
	return nil
}

var errTreeLink = errors.New("cannot hardlink complete cache tree")

// The caller pins and gates snapshots, stops source workspace processes, and
// supplies an unpublished destination. WalkDir never follows directory links.
func copyTree(ctx context.Context, source, destination string, linked bool, fingerprint *string) error {
	for _, dir := range []string{source, destination} {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("cache tree %q must be a real directory", dir)
		}
	}
	h := sha256.New()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	files := make(chan *treeFile, 32)
	var records []*treeFile
	hasRegularFiles := false
	type directoryMode struct {
		path string
		mode fs.FileMode
	}
	var directories []directoryMode
	var workers sync.WaitGroup
	var cloneUnavailable atomic.Bool
	for range 16 {
		workers.Go(func() {
			for file := range files {
				if err := copyTreeFile(ctx, *file, linked, &cloneUnavailable); err != nil {
					cancel(err)
					return
				}
				if fingerprint != nil && !linked {
					info, err := os.Lstat(file.dest)
					if err != nil {
						cancel(err)
						return
					}
					file.info = info
				}
			}
		})
	}
	walkErr := filepath.WalkDir(source, func(full string, entry fs.DirEntry, err error) error {
		name, _ := filepath.Rel(source, full)
		dest := filepath.Join(destination, name)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		record := &treeFile{name: name, full: full, dest: dest, info: info}
		if fingerprint != nil {
			records = append(records, record)
		}
		switch {
		case info.IsDir():
			directories = append(directories, directoryMode{dest, info.Mode()})
			if name == "." {
				return nil
			}
			return os.Mkdir(dest, info.Mode().Perm()|0700)
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			record.target = target
			return os.Symlink(target, dest)
		case info.Mode().IsRegular():
			hasRegularFiles = true
			select {
			case files <- record:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		default:
			return fmt.Errorf("unsupported installed cache entry %q", name)
		}
	})
	close(files)
	workers.Wait()
	if err := errors.Join(walkErr, context.Cause(ctx)); err != nil {
		return err
	}
	// Parents remain writable until their contents are complete.
	for i := len(directories) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Chmod(directories[i].path, directories[i].mode); err != nil {
			return err
		}
	}
	if fingerprint != nil {
		// No regular files means no shared inodes, even if the attempted
		// strategy was linking. Future installed files need private metadata
		// comparison, and publication must not assume linking was supported.
		linked = linked && hasRegularFiles
		for _, record := range records {
			if err := hashTreeEntry(h, record.name, record.info, record.target, linked); err != nil {
				return err
			}
		}
		*fingerprint = treeDigest(h, linked)
	}
	return nil
}

type treeReader struct {
	ctx context.Context
	io.Reader
}

func (r *treeReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

type treeFile struct {
	name, full, dest string
	target           string
	info             fs.FileInfo
}

func preserveTreeFile(dest string, info fs.FileInfo) error {
	return errors.Join(os.Chmod(dest, info.Mode()), os.Chtimes(dest, info.ModTime(), info.ModTime()))
}
func copyTreeFile(ctx context.Context, file treeFile, linked bool, cloneUnavailable *atomic.Bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name, full, dest, info := file.name, file.full, file.dest, file.info

	if linked {
		if err := os.Link(full, dest); err != nil {
			return errors.Join(errTreeLink, err)
		}
		return nil
	}
	if !cloneUnavailable.Load() {
		if err := cloneTreeFile(full, dest, info); err == nil {
			return nil
		} else if errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.ENOSYS) {
			// Cache only within this copy. Mounts and their capabilities can
			// change between jobs; names alone never establish clone support.
			cloneUnavailable.Store(true)
		}
		_ = os.Remove(dest)
	}

	f, err := os.OpenFile(full, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return fmt.Errorf("installed cache file changed: %s", name)
	}
	g, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(g, &treeReader{ctx, f})
	if err := errors.Join(err, g.Chmod(info.Mode()), g.Close()); err != nil {
		return err
	}
	return os.Chtimes(dest, info.ModTime(), info.ModTime())
}
