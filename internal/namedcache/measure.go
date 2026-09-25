package namedcache

import (
	"context"
	"errors"
	"os"
	"sort"
	"time"
)

// legacySizesMarker records that tree sizes stored before releases marked them
// stale were invalidated once. Inventory and GC ignore files at the root.
const legacySizesMarker = ".tree-sizes-stale-on-release"

// MeasureUnknown records sizes for idle tree caches whose size is unknown or
// stale, measured as GC measures them, so inventory reports what GC would
// count without collecting anything. include selects entries; the daemon
// passes its owner filter. No new measurement starts after startBy, and one
// in progress stops only with ctx, so each call makes progress on large
// stores. Unreadable caches stay unmeasured: only GC retires damaged data.
func (s *Store) MeasureUnknown(ctx context.Context, include func(Entry) bool, startBy time.Time) error {
	s.operations.RLock()
	defer s.operations.RUnlock()
	// Concurrent reads would repeat the same walks. One that finds another
	// read measuring reports the sizes known so far.
	if !s.measureMu.TryLock() {
		return nil
	}
	defer s.measureMu.Unlock()
	if err := s.invalidateLegacySizes(ctx, startBy); err != nil {
		return err
	}
	entries, err := s.Inventory(ctx)
	if err != nil {
		return err
	}
	var stale []Entry
	for _, entry := range entries {
		if entry.Tree && entry.BytesUnknown && !entry.Protected() && include(entry) {
			stale = append(stale, entry)
		}
	}
	// Resume after the cache the previous call started, so a cache that is
	// slow to fail cannot use up every call's budget ahead of the others.
	start := sort.Search(len(stale), func(i int) bool { return stale[i].Key.hash() > s.measureCursor })
	for i := range stale {
		entry := stale[(start+i)%len(stale)]
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.now().After(startBy) {
			return nil
		}
		s.measureCursor = entry.Key.hash()
		// Orphaned generations are temporaries GC reclaims without counting.
		size, err := s.measureTreeCache(ctx, entry.Key.hash(), true)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		if err := s.recordMeasurement(ctx, entry, size); err != nil {
			return err
		}
	}
	return nil
}

// Releases before this marker kept a size GC had recorded even after the job
// changed the tree. Mark those sizes stale once, one record at a time and
// within the measurement budget, so inventory measures them again.
func (s *Store) invalidateLegacySizes(ctx context.Context, startBy time.Time) error {
	if s.legacySizesChecked {
		return nil
	}
	if _, err := s.root.Lstat(legacySizesMarker); err == nil {
		s.legacySizesChecked = true
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	entries, err := s.Inventory(ctx)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		// A held tree is marked stale when its last holder releases it.
		if !entry.Tree || entry.BytesUnknown || entry.Protected() {
			continue
		}
		if s.now().After(startBy) {
			return nil
		}
		if err := s.markSizeStale(ctx, entry.Key.hash()); err != nil {
			return err
		}
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	marker, err := s.root.OpenFile(legacySizesMarker, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := errors.Join(marker.Sync(), marker.Close()); err != nil {
		return err
	}
	if err := s.sync("."); err != nil {
		return err
	}
	s.legacySizesChecked = true
	return nil
}

func (s *Store) markSizeStale(ctx context.Context, name string) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	r, err := s.read(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !r.Tree || r.BytesUnknown || r.Protected() {
		return nil
	}
	r.BytesUnknown = true
	return s.write(name, r)
}

// A measurement is published only if the cache stayed idle and unused while
// it was taken, the same check GC makes before recording sizes.
func (s *Store) recordMeasurement(ctx context.Context, sample Entry, size int64) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	name := sample.Key.hash()
	r, err := s.read(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !r.Tree || r.Protected() || !r.LastUsed.Equal(sample.LastUsed) {
		return nil
	}
	r.Bytes, r.BytesUnknown = size, false
	return s.write(name, r)
}
