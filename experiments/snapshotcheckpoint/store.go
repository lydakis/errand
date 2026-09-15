//go:build darwin || linux

package snapshotcheckpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/lydakis/errand/internal/archive"
	"github.com/lydakis/errand/internal/proto"
	"golang.org/x/sys/unix"
)

const magic = "ERRAND-OBS-2\n"
const maxBytes = 64 << 20
const maxEntries = 200000

// The bounded, checksummed payload is disposable after machine failure. Its
// private directory belongs to the caller, not to repository configuration.
func readCheckpoint(ctx context.Context, dir string, key identity) (cp checkpoint, status string, size int64, err error) {
	// Cancellation is not cache corruption and must not start a cold rebuild.
	defer func() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			cp, status, err = checkpoint{}, "", ctxErr
		}
	}()
	if err := ctx.Err(); err != nil {
		return checkpoint{}, "", 0, err
	}
	fd, err := unix.Open(filepath.Join(dir, "checkpoint"), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return checkpoint{}, "missing", 0, nil
	}
	f := os.NewFile(uintptr(fd), "checkpoint")
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBytes {
		return checkpoint{}, "corrupt", 0, nil
	}
	data, err := io.ReadAll(contextReader{ctx, io.LimitReader(f, maxBytes+1)})
	size = int64(len(data))
	bad := func() (checkpoint, string, int64, error) { return checkpoint{}, "corrupt", size, nil }
	if err != nil || len(data) > maxBytes || len(data) < len(magic)+sha256.Size || !bytes.HasPrefix(data, []byte(magic)) {
		return bad()
	}
	payload := data[len(magic) : len(data)-sha256.Size]
	sum, err := checkpointChecksum(ctx, data[:len(data)-sha256.Size])
	if err != nil || !bytes.Equal(sum[:], data[len(data)-sha256.Size:]) {
		return bad()
	}
	cp, err = decodeCheckpoint(ctx, bytes.NewReader(payload))
	if err != nil {
		return bad()
	}
	if cp.Identity != key {
		return checkpoint{}, "identity", size, nil
	}
	m := proto.Manifest{Entries: make([]proto.ManifestEntry, len(cp.Entries))}
	for i, entry := range cp.Entries {
		if err := ctx.Err(); err != nil {
			return checkpoint{}, "", size, err
		}
		m.Entries[i] = entry.Entry
	}
	if err := archive.ValidateSortedContext(ctx, m); err != nil {
		return bad()
	}
	return cp, "hit", size, nil
}

func writeCheckpoint(ctx context.Context, dir string, cp checkpoint) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(cp.Entries) > maxEntries {
		return 0, fmt.Errorf("checkpoint entry limit")
	}
	var buffer bytes.Buffer
	buffer.WriteString(magic)
	if err := encodeCheckpoint(ctx, &buffer, cp); err != nil {
		return 0, err
	}
	if buffer.Len()+sha256.Size > maxBytes {
		return 0, fmt.Errorf("checkpoint byte limit")
	}
	sum, err := checkpointChecksum(ctx, buffer.Bytes())
	if err != nil {
		return 0, err
	}
	buffer.Write(sum[:])
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return 0, err
	}
	fd, err := unix.Open(filepath.Join(dir, "lock"), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return 0, err
	}
	lock := os.NewFile(uintptr(fd), "checkpoint lock")
	defer lock.Close()
	// A busy writer only loses a cache update. Snapshots never wait for this
	// cache, and the last complete writer is safe because every reader revalidates.
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return 0, err
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	temporary := filepath.Join(dir, ".checkpoint.tmp")
	if err := os.Remove(temporary); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	f, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return 0, err
	}
	defer os.Remove(temporary)
	_, writeErr := io.Copy(f, contextReader{ctx, bytes.NewReader(buffer.Bytes())})
	if err := errors.Join(writeErr, f.Close(), ctx.Err()); err != nil {
		return 0, err
	}
	// No fsync: this is not a durable receipt or source of truth. An interrupted
	// write leaves the old generation, or a missing/corrupt cache that rebuilds.
	if err := os.Rename(temporary, filepath.Join(dir, "checkpoint")); err != nil {
		return 0, err
	}
	return int64(buffer.Len()), nil
}
