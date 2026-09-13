package changes

import (
	"context"
	"fmt"
	"slices"

	"github.com/lydakis/errand/internal/manifest"
	"github.com/lydakis/errand/internal/proto"
)

// SnapshotDelta owns a comparison of two immutable source observations. Its
// bundle is canonical because it was derived here, rather than received on wire.
type SnapshotDelta struct {
	base, current *manifest.Snapshot
	bundle        proto.ChangeBundle
}

// PrepareSnapshotDelta retains inventories through the same selection logic as
// one-shot push and fetch. Snapshot identity is still the wire identity.
func PrepareSnapshotDelta(ctx context.Context, base, current *manifest.Snapshot, maxBytes int64) (*SnapshotDelta, error) {
	bundle, err := workspaceSnapshotDelta(ctx, base, current, maxBytes)
	if err != nil {
		return nil, err
	}
	return &SnapshotDelta{base: base, current: current, bundle: bundle}, nil
}

func (d *SnapshotDelta) Bundle() proto.ChangeBundle { return cloneSourceDelta(d.bundle) }

// Accepted reuses the current snapshot when the receipt accepts every root.
// Partial receipts update only accepted paths and preserve the other bases.
func (d *SnapshotDelta) Accepted(ctx context.Context, applied []string) (*manifest.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if slices.Equal(applied, d.bundle.Paths) {
		if len(d.bundle.Paths) == 0 {
			return d.base, nil
		}
		if d.current.Len() == 0 {
			// Checkpoint advancement canonicalizes actual deletion to nil entries.
			return manifest.New(ctx, proto.Manifest{})
		}
		return d.current, nil
	}
	return AcceptedSnapshotDelta(ctx, d.base, d.bundle, applied)
}

// AcceptedSnapshotDelta advances source observations after a successful receipt.
// It never incorporates live or merged destination values. The baseline and
// every claimed merge base are checked before publishing an updated index.
func AcceptedSnapshotDelta(ctx context.Context, base *manifest.Snapshot, delta proto.ChangeBundle, applied []string) (*manifest.Snapshot, error) {
	if err := ValidateBundleContext(ctx, delta); err != nil {
		return nil, err
	}
	root, err := base.RootHash(ctx)
	if err != nil {
		return nil, err
	}
	if root != delta.BaselineRoot {
		return nil, ErrCheckpointChanged
	}
	before, err := manifest.New(ctx, delta.BaseManifest)
	if err != nil {
		return nil, err
	}
	after, err := manifest.New(ctx, delta.RemoteManifest)
	if err != nil {
		return nil, err
	}
	selected := make(map[string]bool, len(applied))
	for _, name := range applied {
		if _, ok := slices.BinarySearch(delta.Paths, name); !ok {
			return nil, fmt.Errorf("receipt names an unknown source change")
		}
		selected[name] = true
	}
	edits := make(map[string]manifest.Edit)
	for _, name := range delta.Paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		metadata := bundleHasMetadataPath(delta, name)
		collect := func(s *manifest.Snapshot) ([]proto.ManifestEntry, error) {
			var entries []proto.ManifestEntry
			if metadata {
				if e, ok := s.Lookup(name); ok {
					entries = append(entries, e)
				}
				return entries, ctx.Err()
			}
			err := s.Subtree(ctx, name, func(e proto.ManifestEntry) bool { entries = append(entries, e); return true })
			return entries, err
		}
		actual, err := collect(base)
		if err != nil {
			return nil, err
		}
		claimed, err := collect(before)
		if err != nil {
			return nil, err
		}
		if !slices.Equal(actual, claimed) {
			return nil, fmt.Errorf("bundle base for %q does not match checkpoint", name)
		}
		if !selected[name] {
			continue
		}
		for _, e := range actual {
			edits[e.Path] = manifest.Edit{Entry: e, Delete: true}
		}
		replacements, err := collect(after)
		if err != nil {
			return nil, err
		}
		for _, e := range replacements {
			edits[e.Path] = manifest.Edit{Entry: e}
		}
	}
	batch := make([]manifest.Edit, 0, len(edits))
	for _, edit := range edits {
		batch = append(batch, edit)
	}
	return base.Update(ctx, batch)
}
