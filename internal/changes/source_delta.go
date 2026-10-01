package changes

import (
	"context"
	"fmt"
	"slices"

	"github.com/lydakis/errand/internal/manifest"
	"github.com/lydakis/errand/internal/proto"
)

// PrepareSourceDelta identifies complete changed roots and their ancestors.
func PrepareSourceDelta(ctx context.Context, base, current proto.Manifest, maxBytes int64) (proto.ChangeBundle, error) {
	return workspaceDelta(ctx, base, current, maxBytes)
}

// ExpandSourceDelta reconstructs metadata from a retained checkpoint, never
// from live destination files. Recomputing the delta validates change roots,
// structural replacements, directory metadata, and all claimed merge bases.
func ExpandSourceDelta(base proto.Manifest, delta proto.ChangeBundle, root string, maxBytes int64) (proto.Manifest, error) {
	return ExpandSourceDeltaContext(context.Background(), base, delta, root, maxBytes)
}

// ExpandSourceDeltaContext rejects malformed roots before reconstructing source
// metadata and honors cancellation throughout validation and reconstruction.
func ExpandSourceDeltaContext(ctx context.Context, base proto.Manifest, delta proto.ChangeBundle, root string, maxBytes int64) (proto.Manifest, error) {
	state, err := expandSourceSnapshot(ctx, NewSourceBase(base), delta, root, maxBytes)
	if err != nil {
		return proto.Manifest{}, err
	}
	return state.Manifest(ctx)
}

// The base's retained identity stands in for hashing before, which holds a copy
// of exactly the base's entries. A checkpoint base shares it with the stage.
func expandSourceSnapshot(ctx context.Context, base *SourceBase, delta proto.ChangeBundle, root string, maxBytes int64) (*manifest.Snapshot, error) {
	if err := ValidateBundleContext(ctx, delta); err != nil {
		return nil, err
	}
	if maxBytes >= 0 && delta.Bytes > maxBytes {
		return nil, ErrByteLimitExceeded
	}

	before, err := manifest.New(ctx, base.manifest)
	if err != nil {
		return nil, err
	}
	baselineRoot := base.rootHash()
	if baselineRoot != delta.BaselineRoot {
		return nil, ErrCheckpointChanged
	}
	states := make(map[string]string)
	for _, p := range delta.Paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		states[p] = "accepted"
	}
	state, err := acceptedSourceSnapshotContext(ctx, base.manifest, delta, transferOutcome{States: states})
	if err != nil {
		return nil, err
	}
	sourceRoot, err := state.RootHash(ctx)
	if err != nil {
		return nil, err
	}
	if sourceRoot != root {
		return nil, fmt.Errorf("source delta does not reconstruct the declared manifest")
	}
	expected, err := selectSnapshotDelta(ctx, before, state, maxBytes)
	if err != nil {
		return nil, err
	}
	expected.BaselineRoot = baselineRoot
	if expected.RootHash() != delta.RootHash() {
		return nil, fmt.Errorf("source delta differs from checkpoint changes")
	}
	return state, nil
}

// AcceptedSource mirrors checkpoint advancement after a successful receipt.
// It is metadata only; callers must not use destination contents as source.
func AcceptedSource(base, current proto.Manifest, applied []string) (proto.Manifest, error) {
	return AcceptedSourceContext(context.Background(), base, current, applied)
}

func AcceptedSourceContext(ctx context.Context, base, current proto.Manifest, applied []string) (proto.Manifest, error) {
	b, err := workspaceDelta(ctx, base, current, -1)
	if err != nil {
		return proto.Manifest{}, err
	}
	return acceptSourceDelta(ctx, base, b, applied)
}

// AcceptedSourceDelta reuses a previously prepared delta after its receipt.
// The bundle and its merge bases are still checked; destination hashes never
// enter the accepted-source checkpoint.
func AcceptedSourceDelta(ctx context.Context, base proto.Manifest, delta proto.ChangeBundle, applied []string) (proto.Manifest, error) {
	if err := ValidateBundleContext(ctx, delta); err != nil {
		return proto.Manifest{}, err
	}
	if base.RootHash() != delta.BaselineRoot {
		return proto.Manifest{}, ErrCheckpointChanged
	}
	return acceptSourceDelta(ctx, base, delta, applied)
}

func acceptSourceDelta(ctx context.Context, base proto.Manifest, b proto.ChangeBundle, applied []string) (proto.Manifest, error) {
	states := make(map[string]string)
	for _, p := range applied {
		if err := ctx.Err(); err != nil {
			return proto.Manifest{}, err
		}
		if _, ok := slices.BinarySearch(b.Paths, p); !ok {
			return proto.Manifest{}, fmt.Errorf("receipt names an unknown source change")
		}
		states[p] = "accepted"
	}
	return acceptedSourceManifestContext(ctx, base, b, transferOutcome{States: states})
}
