package namedcache

import (
	"context"
	"os"
	"time"
)

// MeasureUnknown records sizes for idle tree caches whose size is unknown or
// stale, measured as GC measures them, so inventory reports what GC would
// count without collecting anything. include selects entries; the daemon
// passes its owner filter. No new measurement starts after startBy, and one
// in progress stops only with ctx, so each call makes progress on large
// stores. Unreadable caches stay unmeasured: only GC retires damaged data.
func (s *Store) MeasureUnknown(ctx context.Context, include func(Entry) bool, startBy time.Time) error {
	s.operations.RLock()
	defer s.operations.RUnlock()
	entries, err := s.Inventory(ctx)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Tree || !entry.BytesUnknown || entry.Protected() || !include(entry) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(startBy) {
			return nil
		}
		size, err := s.measureTreeCache(ctx, entry.Key.hash(), false)
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
