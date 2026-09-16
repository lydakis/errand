//go:build darwin || linux

package snapshotcheckpoint

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lydakis/errand/internal/manifest"
	"github.com/lydakis/errand/internal/snapshot"
	"golang.org/x/sys/unix"
)

const derivedMagic = "ERRAND-INDEX-1\n"
const maxJournalRecords = 32
const maxJournalBytes = 8 << 20
const frameOverhead = 8 + 32 + 32 // length, previous digest, digest

// One bounded file holds a complete base and checksummed, chained transactions.
// A torn suffix recovers only complete earlier frames. Compaction atomically
// replaces the file. Readers/writers take nonblocking shared/exclusive locks;
// a busy cache is expendable and never delays snapshot work.
func derivedLock(dir string, write bool) (*os.File, error) {
	if write {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	flags := unix.O_RDWR | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	if write {
		flags |= unix.O_CREAT
	}
	fd, err := unix.Open(filepath.Join(dir, "index.lock"), flags, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "index lock")
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("invalid index lock")
	}
	mode := unix.LOCK_SH
	if write {
		mode = unix.LOCK_EX
	}
	if err := unix.Flock(fd, mode|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func unlockDerived(f *os.File) { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }

func readDerivedFile(ctx context.Context, dir string) ([]byte, error) {
	fd, err := unix.Open(filepath.Join(dir, "index"), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "index")
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxBytes {
		return nil, fmt.Errorf("invalid index file")
	}
	data, err := io.ReadAll(contextReader{ctx, io.LimitReader(f, maxBytes+1)})
	if len(data) > maxBytes {
		return nil, fmt.Errorf("checkpoint byte limit")
	}
	return data, err
}

func readDerived(ctx context.Context, dir string, key identity) (loaded loadedCheckpoint, status string, size int64, err error) {
	defer func() {
		if e := ctx.Err(); e != nil {
			loaded, status, err = loadedCheckpoint{}, "", e
		}
	}()
	lock, e := derivedLock(dir, false)
	if e != nil {
		return loadedCheckpoint{}, "missing", 0, nil
	}
	defer unlockDerived(lock)
	data, e := readDerivedFile(ctx, dir)
	if os.IsNotExist(e) {
		return loadedCheckpoint{}, "missing", 0, nil
	}
	if e != nil || !bytes.HasPrefix(data, []byte(derivedMagic)) {
		return loadedCheckpoint{}, "corrupt", int64(len(data)), nil
	}
	size = int64(len(data))
	pos, frames := len(derivedMagic), 0
	baseEnd := 0
	var chain [32]byte
	var pending map[string]observationEdit
	for pos < len(data) {
		if e := ctx.Err(); e != nil {
			return loadedCheckpoint{}, "", size, e
		}
		remaining := data[pos:]
		if len(remaining) < frameOverhead || frames > maxJournalRecords {
			break
		}
		length := binary.LittleEndian.Uint64(remaining)
		if length > uint64(len(remaining)-frameOverhead) {
			break
		}
		end := 8 + 32 + int(length)
		if !bytes.Equal(remaining[8:40], chain[:]) {
			break
		}
		sum, e := checkpointChecksum(ctx, remaining[:end])
		if e != nil || !bytes.Equal(sum[:], remaining[end:end+32]) {
			break
		}
		next, e := decodeDerived(ctx, remaining[40:end], key, loaded, frames > 0)
		if e != nil {
			break
		}
		loaded = next
		if len(next.pendingEdits) > 0 {
			if pending == nil {
				pending = make(map[string]observationEdit, len(next.pendingEdits))
			}
			for _, edit := range next.pendingEdits {
				pending[edit.Value.Entry.Path] = edit
			}
			loaded.pendingEdits = nil
		}
		chain = sum
		pos += end + 32
		frames++
		if frames == 1 {
			baseEnd = pos
		}
	}
	if frames == 0 {
		return loadedCheckpoint{}, "corrupt", size, nil
	}
	if len(pending) > 0 {
		edits := make([]observationEdit, 0, len(pending))
		for name, edit := range pending {
			if err := ctx.Err(); err != nil {
				return loadedCheckpoint{}, "", size, err
			}
			if edit.Delete {
				_, found := slices.BinarySearchFunc(loaded.Entries, name, func(o observation, p string) int { return strings.Compare(o.Entry.Path, p) })
				if !found {
					continue
				} // Created and deleted within the journal.
			}
			edits = append(edits, edit)
		}
		slices.SortFunc(edits, func(a, b observationEdit) int { return strings.Compare(a.Value.Entry.Path, b.Value.Entry.Path) })
		loaded.Entries, err = applyObservations(ctx, loaded.Entries, edits)
		if err != nil {
			return loadedCheckpoint{}, "corrupt", size, nil
		}
	}
	loaded.chain, loaded.diskSize, loaded.journalRecords = chain, len(data), frames-1
	loaded.journalBytes = pos - baseEnd
	loaded.diskDigest, err = checkpointChecksum(ctx, data)
	loaded.recovered = pos != len(data)
	status = "hit"
	if loaded.recovered {
		status = "recovered"
	}
	return loaded, status, size, err
}

func derivedFrame(ctx context.Context, payload []byte, previous [32]byte) ([]byte, error) {
	if len(payload) > maxBytes-len(derivedMagic)-frameOverhead {
		return nil, fmt.Errorf("checkpoint byte limit")
	}
	frame := make([]byte, 0, frameOverhead+len(payload))
	frame = binary.LittleEndian.AppendUint64(frame, uint64(len(payload)))
	frame = append(frame, previous[:]...)
	frame = append(frame, payload...)
	sum, err := checkpointChecksum(ctx, frame)
	return append(frame, sum[:]...), err
}

func writeDerived(ctx context.Context, dir string, key identity, entries snapshot.VerifiedObservations, state *manifest.Snapshot, prior loadedCheckpoint, journal bool) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	delta := journal && prior.state != nil && !prior.recovered && prior.journalRecords < maxJournalRecords
	payload, err := encodeDerived(ctx, key, entries, state, prior, delta)
	if err != nil {
		return 0, false, err
	}
	previous := prior.chain
	if delta && (prior.journalBytes+len(payload)+frameOverhead > maxJournalBytes || prior.diskSize+len(payload)+frameOverhead > maxBytes) {
		delta = false
		payload, err = encodeDerived(ctx, key, entries, state, prior, false)
		if err != nil {
			return 0, false, err
		}
	}
	if !delta {
		previous = [32]byte{}
	}
	frame, err := derivedFrame(ctx, payload, previous)
	if err != nil {
		return 0, false, err
	}
	lock, err := derivedLock(dir, true)
	if err != nil {
		return 0, false, err
	}
	defer unlockDerived(lock)
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	// A read/build/write gap must not append against a different generation.
	// Include the read/check cost in measurements. Full replacements are safe
	// last-writer-wins advisory snapshots; deltas require their exact base.
	if delta {
		current, err := readDerivedFile(ctx, dir)
		if err != nil {
			return 0, false, err
		}
		sum, err := checkpointChecksum(ctx, current)
		if err != nil {
			return 0, false, err
		}
		if sum != prior.diskDigest {
			return 0, false, fmt.Errorf("checkpoint generation changed")
		}
		fd, err := unix.Open(filepath.Join(dir, "index"), unix.O_WRONLY|unix.O_APPEND|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return 0, false, err
		}
		f := os.NewFile(uintptr(fd), "index journal")
		n, writeErr := io.Copy(f, contextReader{ctx, bytes.NewReader(frame)})
		return n, false, errors.Join(writeErr, f.Close(), ctx.Err())
	}
	tmp := filepath.Join(dir, ".index.tmp")
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return 0, false, err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return 0, false, err
	}
	defer os.Remove(tmp)
	data := append([]byte(derivedMagic), frame...)
	n, writeErr := io.Copy(f, contextReader{ctx, bytes.NewReader(data)})
	if err := errors.Join(writeErr, f.Close(), ctx.Err()); err != nil {
		return 0, false, err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "index")); err != nil {
		return 0, false, err
	}
	return n, prior.state != nil, nil
}
