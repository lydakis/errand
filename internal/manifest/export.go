package manifest

import (
	"context"
	"slices"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

// readyView returns an internal immutable view, never a caller-owned slice.
func (s *Snapshot) readyView() (proto.Manifest, bool) {
	if s.entries != nil {
		return proto.Manifest{Entries: s.entries}, true
	}
	if m := s.view.Load(); m != nil {
		return *m, true
	}
	if s.count == 0 {
		return proto.Manifest{}, true
	}
	return proto.Manifest{}, false
}

// materialized caches the flat representation required by the wire protocol.
// If the prior snapshot already had a view, copy its unchanged ranges rather
// than traversing the tree again. Only the array is retained, not a history of
// prior snapshots. Updates without an intervening export keep the tree path.
func (s *Snapshot) materialized(ctx context.Context) (proto.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return proto.Manifest{}, err
	}
	if m, ok := s.readyView(); ok {
		return m, nil
	}
	var entries []proto.ManifestEntry
	if s.viewEdits != nil {
		m, err := editedView(ctx, s.viewBase, s.viewEdits, s.count)
		if err != nil {
			return proto.Manifest{}, err
		}
		entries = m.Entries
	} else {
		entries = make([]proto.ManifestEntry, 0, s.count)
		if err := s.Walk(ctx, func(e proto.ManifestEntry) bool { entries = append(entries, e); return true }); err != nil {
			return proto.Manifest{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return proto.Manifest{}, err
	}
	m := &proto.Manifest{Entries: entries}
	s.view.CompareAndSwap(nil, m)
	return *s.view.Load(), nil
}

// editedView copies unchanged ranges from an already validated inventory.
// Its caller validates replacement metadata and the resulting hierarchy.
func editedView(ctx context.Context, base []proto.ManifestEntry, edits []Edit, count int) (proto.Manifest, error) {
	var entries []proto.ManifestEntry
	if count > 0 {
		entries = make([]proto.ManifestEntry, 0, count)
	}
	remaining := base
	for _, edit := range edits {
		if err := ctx.Err(); err != nil {
			return proto.Manifest{}, err
		}
		i, exists := slices.BinarySearchFunc(remaining, edit.Entry.Path, func(e proto.ManifestEntry, p string) int { return strings.Compare(e.Path, p) })
		entries = append(entries, remaining[:i]...)
		remaining = remaining[i:]
		if exists {
			remaining = remaining[1:]
		}
		if !edit.Delete {
			entries = append(entries, edit.Entry)
		}
	}
	entries = append(entries, remaining...)
	return proto.Manifest{Entries: entries}, ctx.Err()
}
