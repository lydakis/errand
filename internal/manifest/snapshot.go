// Package manifest owns immutable, validated source metadata. It does not grant
// selection authority or certify that live files still match the snapshot.
package manifest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/lydakis/errand/internal/archive"
	"github.com/lydakis/errand/internal/proto"
)

// Edit replaces or deletes one exact path. Descendant deletions are explicit.
// Update applies the whole batch before checking hierarchy and byte totals.
type Edit struct {
	Entry  proto.ManifestEntry
	Delete bool
}

// Below 4K entries, native benchmarks favor contiguous updates over indexing.
const minIndexedEntries = 4096

// Snapshot has a flat cold representation and a lazily retained tree. Metadata
// is immutable; only derived caches mutate, under synchronization. Constructors
// own their input, and exported manifests always own their slices.
type Snapshot struct {
	entries   []proto.ManifestEntry
	index     atomic.Pointer[node]
	noIndex   atomic.Bool
	size      int64
	count     int
	mu        sync.Mutex
	wireRoot  string
	view      atomic.Pointer[proto.Manifest]
	viewBase  []proto.ManifestEntry
	viewEdits []Edit
}

func New(ctx context.Context, m proto.Manifest) (*Snapshot, error) {
	size, err := validate(ctx, m)
	if err != nil {
		return nil, err
	}
	return &Snapshot{entries: slices.Clone(m.Entries), size: size, count: len(m.Entries)}, ctx.Err()
}

// Validate checks the same metadata invariants as New without retaining a copy.
// It does not confer ownership or validate live filesystem contents.
func Validate(ctx context.Context, m proto.Manifest) error {
	_, err := validate(ctx, m)
	return err
}

func validate(ctx context.Context, m proto.Manifest) (int64, error) {
	if err := archive.ValidateSortedContext(ctx, m); err != nil {
		return 0, err
	}
	var size int64
	for _, e := range m.Entries {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if e.Path == "." {
			return 0, fmt.Errorf("manifest paths must be strictly sorted and relative")
		}
		if e.Size > math.MaxInt64-size {
			return 0, fmt.Errorf("manifest byte total overflows")
		}
		size += e.Size
	}
	return size, ctx.Err()
}

func (s *Snapshot) Bytes() int64 { return s.size }
func (s *Snapshot) Len() int     { return s.count }
func (s *Snapshot) Lookup(name string) (proto.ManifestEntry, bool) {
	if s.entries != nil {
		i, ok := slices.BinarySearchFunc(s.entries, name, func(e proto.ManifestEntry, p string) int { return strings.Compare(e.Path, p) })
		if ok {
			return s.entries[i], true
		}
		return proto.ManifestEntry{}, false
	}
	for n := s.index.Load(); n != nil; {
		if name == n.entry.Path {
			return n.entry, true
		}
		if name < n.entry.Path {
			n = n.left
		} else {
			n = n.right
		}
	}
	return proto.ManifestEntry{}, false
}

// Walk visits sorted entries. Returning false stops the walk successfully.
func (s *Snapshot) Walk(ctx context.Context, visit func(proto.ManifestEntry) bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.entries != nil {
		for _, e := range s.entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !visit(e) {
				break
			}
		}
		return ctx.Err()
	}
	walkRange(ctx, s.index.Load(), "", "", visit)
	return ctx.Err()
}

// Subtree visits the exact root and all descendants, including implicit roots.
func (s *Snapshot) Subtree(ctx context.Context, root string, visit func(proto.ManifestEntry) bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e, ok := s.Lookup(root); ok && !visit(e) {
		return ctx.Err()
	}
	lo, hi := root+"/", root+"0" // '/' + 1 is the exclusive prefix bound.
	if s.entries != nil {
		i := sort.Search(len(s.entries), func(i int) bool { return s.entries[i].Path >= lo })
		for ; i < len(s.entries) && s.entries[i].Path < hi; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !visit(s.entries[i]) {
				break
			}
		}
	} else {
		walkRange(ctx, s.index.Load(), lo, hi, visit)
	}
	return ctx.Err()
}
func walkRange(ctx context.Context, n *node, lo, hi string, visit func(proto.ManifestEntry) bool) bool {
	if n == nil || ctx.Err() != nil {
		return ctx.Err() == nil
	}
	if n.entry.Path >= lo && !walkRange(ctx, n.left, lo, hi, visit) {
		return false
	}
	if n.entry.Path >= lo && (hi == "" || n.entry.Path < hi) && !visit(n.entry) {
		return false
	}
	if hi == "" || n.entry.Path < hi {
		return walkRange(ctx, n.right, lo, hi, visit)
	}
	return true
}

func (s *Snapshot) Manifest(ctx context.Context) (proto.Manifest, error) {
	m, err := s.materialized(ctx)
	if err != nil {
		return proto.Manifest{}, err
	}
	result := proto.Manifest{Entries: slices.Clone(m.Entries)}
	return result, ctx.Err()
}

// RootHash is the existing JSON wire identity, cached once per immutable state.
// Internal tree hashes are never substituted for this protocol value.
func (s *Snapshot) RootHash(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.wireRoot == "" {
		m, err := s.materialized(ctx)
		if err != nil {
			return "", err
		}
		hash := m.RootHash()
		if err := ctx.Err(); err != nil {
			return "", err
		}
		s.wireRoot = hash
	}
	return s.wireRoot, nil
}

func (s *Snapshot) tree(ctx context.Context) (*node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if n := s.index.Load(); n != nil {
		return n, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.index.Load(); n != nil {
		return n, ctx.Err()
	}
	n, err := buildTree(ctx, s.entries)
	if err != nil {
		return nil, err
	}
	s.index.Store(n)
	return n, nil
}

// PrepareUpdates pays the one-time indexing cost before a latency-sensitive
// update loop. Cold one-shot consumers do not need it. Chosen path priorities
// may require flat updates; that valid fallback is transparent to callers.
func (s *Snapshot) PrepareUpdates(ctx context.Context) error {
	if s.count < minIndexedEntries && s.index.Load() == nil {
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.noIndex.Load() {
		return nil
	}
	_, err := s.tree(ctx)
	if errors.Is(err, errTreeHeight) {
		s.noIndex.Store(true)
		return ctx.Err()
	}
	return err
}

func (s *Snapshot) Update(ctx context.Context, edits []Edit) (*Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(edits) == 0 {
		return s, nil
	}
	ordered := slices.Clone(edits)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Entry.Path < ordered[j].Entry.Path })
	var replacements proto.Manifest
	size := s.size
	count := s.count
	changed := false
	structural := false
	typesChanged := false
	for i, e := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.Entry.Path == "" || e.Entry.Path == "." || path.Clean(e.Entry.Path) != e.Entry.Path || strings.HasPrefix(e.Entry.Path, "/") || strings.HasPrefix(e.Entry.Path, "../") || e.Entry.Path == ".." || strings.ContainsRune(e.Entry.Path, 0) {
			return nil, fmt.Errorf("unsafe edit path")
		}
		if i > 0 && ordered[i-1].Entry.Path == e.Entry.Path {
			return nil, fmt.Errorf("duplicate edit path")
		}
		if old, ok := s.Lookup(e.Entry.Path); ok {
			structural = structural || e.Delete
			typesChanged = typesChanged || old.Type != e.Entry.Type
			if e.Delete {
				count--
			}
			size -= old.Size
			changed = changed || e.Delete || old != e.Entry
		} else {
			structural = true
			if !e.Delete {
				count++
			}
			changed = changed || !e.Delete
		}
		if !e.Delete {
			replacements.Entries = append(replacements.Entries, e.Entry)
		}
	}
	for _, e := range replacements.Entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := archive.ValidateEntry(e); err != nil {
			return nil, err
		}
	}
	if !changed {
		return s, ctx.Err()
	}
	for _, e := range replacements.Entries {
		if e.Size > math.MaxInt64-size {
			return nil, fmt.Errorf("manifest byte total overflows")
		}
		size += e.Size
	}
	preservesHierarchy := !structural && !typesChanged
	// Both small inventories and height-limited trees use the same validated
	// array update. Internal reads borrow immutable views instead of cloning.
	flatUpdate := func(noIndex bool) (*Snapshot, error) {
		base, err := s.materialized(ctx)
		if err != nil {
			return nil, err
		}
		m, err := editedView(ctx, base.Entries, ordered, count)
		if err != nil {
			return nil, err
		}
		next := &Snapshot{entries: m.Entries, size: size, count: count}
		next.noIndex.Store(noIndex)
		if !preservesHierarchy {
			if err := next.validateReplacements(ctx, replacements.Entries); err != nil {
				return nil, err
			}
		}
		return next, ctx.Err()
	}
	if s.noIndex.Load() {
		return flatUpdate(true)
	}
	if s.index.Load() == nil && s.count < minIndexedEntries && count < minIndexedEntries {
		return flatUpdate(false)
	}
	root, err := s.tree(ctx)
	if errors.Is(err, errTreeHeight) {
		s.noIndex.Store(true)
		return flatUpdate(true)
	}
	if err != nil {
		return nil, err
	}
	if !structural && len(ordered) > 1 {
		root = replaceNodes(ctx, root, ordered)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	} else {
		for _, e := range ordered {
			root = updateNode(ctx, root, e)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if root != nil && root.height > maxHeight {
				return flatUpdate(true)
			}
		}
	}

	next := &Snapshot{size: size}
	if base, ok := s.readyView(); ok {
		next.viewBase, next.viewEdits = base.Entries, ordered
	}
	next.index.Store(root)
	if root != nil {
		next.count = root.count
	}
	// A validated base remains hierarchically valid when every path and type is
	// preserved. Metadata, paths and totals were still checked above. Membership
	// preservation alone (structural == false) does not rule out type changes.
	if !preservesHierarchy {
		if err := next.validateReplacements(ctx, replacements.Entries); err != nil {
			return nil, err
		}
	}
	return next, ctx.Err()
}

func (s *Snapshot) validateReplacements(ctx context.Context, entries []proto.ManifestEntry) error {
	for _, e := range entries {
		for parent := path.Dir(e.Path); parent != "."; parent = path.Dir(parent) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if p, ok := s.Lookup(parent); ok && p.Type != proto.EntryDir {
				return fmt.Errorf("manifest path passes through non-directory %q", parent)
			}
		}
		if e.Type != proto.EntryDir {
			descendant := false
			if err := s.Subtree(ctx, e.Path, func(child proto.ManifestEntry) bool { descendant = child.Path != e.Path; return !descendant }); err != nil {
				return err
			}
			if descendant {
				return fmt.Errorf("manifest non-directory %q has descendants", e.Path)
			}
		}
	}
	return ctx.Err()
}

// Diff returns sorted exact edits. Cold snapshots compare linearly without
// constructing trees. Retained trees skip equal subtrees by their hashes.
func (s *Snapshot) Diff(ctx context.Context, next *Snapshot) ([]Edit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == next {
		return nil, nil
	}
	a, b := s.index.Load(), next.index.Load()
	if a != nil && b != nil || s.entries == nil && next.entries == nil {
		var result []Edit
		diffNodes(ctx, a, b, &result)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return result, nil
	}
	before, after := proto.Manifest{Entries: s.entries}, proto.Manifest{Entries: next.entries}
	var err error
	if s.entries == nil {
		before, err = s.materialized(ctx)
		if err != nil {
			return nil, err
		}
	}
	if next.entries == nil {
		after, err = next.materialized(ctx)
		if err != nil {
			return nil, err
		}
	}
	var result []Edit
	i, j := 0, 0
	for i < len(before.Entries) || j < len(after.Entries) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch {
		case j == len(after.Entries) || i < len(before.Entries) && before.Entries[i].Path < after.Entries[j].Path:
			result = append(result, Edit{Entry: before.Entries[i], Delete: true})
			i++
		case i == len(before.Entries) || after.Entries[j].Path < before.Entries[i].Path:
			result = append(result, Edit{Entry: after.Entries[j]})
			j++
		default:
			if before.Entries[i] != after.Entries[j] {
				result = append(result, Edit{Entry: after.Entries[j]})
			}
			i++
			j++
		}
	}
	return result, nil
}
