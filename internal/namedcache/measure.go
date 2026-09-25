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
	if err := s.invalidateLegacySizes(ctx); err != nil {
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
	s.measureMu.Lock()
	cursor := s.measureCursor
	s.measureMu.Unlock()
	start := sort.Search(len(stale), func(i int) bool { return stale[i].Key.hash() > cursor })
	for i := range stale {
		entry := stale[(start+i)%len(stale)]
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.now().After(startBy) {
			return nil
		}
		s.measureMu.Lock()
		s.measureCursor = entry.Key.hash()
		s.measureMu.Unlock()
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
// changed the tree. Mark those sizes stale once so inventory measures again.
func (s *Store) invalidateLegacySizes(ctx context.Context) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	if s.legacySizesChecked {
		return nil
	}
	if _, err := s.root.Lstat(legacySizesMarker); err == nil {
		s.legacySizesChecked = true
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	entries, err := s.inventory(ctx)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		// A held tree is marked stale when its last holder releases it.
		if !entry.Tree || entry.BytesUnknown || entry.Protected() {
			continue
		}
		r, err := s.read(entry.Key.hash())
		if err != nil {
			return err
		}
		r.BytesUnknown = true
		if err := s.write(entry.Key.hash(), r); err != nil {
			return err
		}
	}
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
