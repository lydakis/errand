package changes

import (
	"context"
	"fmt"
	"slices"

	"github.com/lydakis/errand/internal/manifest"
	"github.com/lydakis/errand/internal/proto"
)

// PreparedTransferSource owns validated source metadata and its canonical
// delta. It contains no live destination values or body-verification evidence.
// Its immutable metadata can cross an upload interval; StagePrepared rechecks
// the receiver checkpoint after recovery and still verifies supplied bodies.
type PreparedTransferSource struct {
	snapshot   *manifest.Snapshot
	delta      proto.ChangeBundle
	sourceRoot string
	valid      bool
}

// Manifest and Delta return copies so callers cannot change the prepared plan.
func (p PreparedTransferSource) Manifest() proto.Manifest {
	if p.snapshot == nil {
		return proto.Manifest{}
	}
	m, _ := p.snapshot.Manifest(context.Background())
	return m
}
func (p PreparedTransferSource) Delta() proto.ChangeBundle { return cloneSourceDelta(p.delta) }

func PrepareTransferSource(ctx context.Context, base, current proto.Manifest, maxBytes int64) (PreparedTransferSource, error) {
	state, err := newSourceSnapshot(ctx, current)
	if err != nil {
		return PreparedTransferSource{}, err
	}
	before, err := manifest.New(ctx, base)
	if err != nil {
		return PreparedTransferSource{}, err
	}
	delta, err := workspaceSnapshotDelta(ctx, before, state, maxBytes)
	if err != nil {
		return PreparedTransferSource{}, err
	}
	if err := ValidateBundleContext(ctx, delta); err != nil {
		return PreparedTransferSource{}, err
	}
	root, err := state.RootHash(ctx)
	if err != nil {
		return PreparedTransferSource{}, err
	}
	return PreparedTransferSource{snapshot: state, delta: cloneSourceDelta(delta), sourceRoot: root, valid: true}, nil
}

func newSourceSnapshot(ctx context.Context, current proto.Manifest) (*manifest.Snapshot, error) {
	state, err := manifest.New(ctx, current)
	if err != nil {
		return nil, err
	}
	var invalid error
	err = state.Walk(ctx, func(e proto.ManifestEntry) bool {
		invalid = validateSourcePath(e.Path)
		return invalid == nil
	})
	if err != nil {
		return nil, err
	}
	if invalid != nil {
		return nil, invalid
	}
	return state, nil
}

func validateSourcePath(name string) error {
	if err := validatePath(name); err != nil {
		return err
	}
	if pathUsesApplyTransaction(name) {
		return fmt.Errorf("invalid checkpoint manifest path %q", name)
	}
	return nil
}

func ExpandTransferSource(ctx context.Context, base proto.Manifest, delta proto.ChangeBundle, sourceRoot string, maxBytes int64) (PreparedTransferSource, error) {
	state, err := expandSourceSnapshot(ctx, base, delta, sourceRoot, maxBytes)
	if err != nil {
		return PreparedTransferSource{}, err
	}
	// Expansion retains the validated state and its cached wire identity.
	return PreparedTransferSource{snapshot: state, delta: cloneSourceDelta(delta), sourceRoot: sourceRoot, valid: true}, ctx.Err()
}

func cloneSourceManifest(m proto.Manifest) proto.Manifest {
	return proto.Manifest{Entries: slices.Clone(m.Entries)}
}
func cloneSourceDelta(b proto.ChangeBundle) proto.ChangeBundle {
	b.Paths = slices.Clone(b.Paths)
	b.MetadataPaths = slices.Clone(b.MetadataPaths)
	b.BaseManifest = cloneSourceManifest(b.BaseManifest)
	b.RemoteManifest = cloneSourceManifest(b.RemoteManifest)
	return b
}

func (s *TransferSession) StagePrepared(ctx context.Context, id, source string, p PreparedTransferSource) (string, proto.ChangeBundle, error) {
	if !p.valid {
		return "", proto.ChangeBundle{}, fmt.Errorf("transfer source was not prepared")
	}

	return s.stage(ctx, id, source, proto.Manifest{}, &p)
}

func (p PreparedTransferSource) validateStageLimits(ctx context.Context, s *TransferSession) error {
	// Source quotas count the complete reconstructed tree, including unchanged
	// cached files. Delta factories may have used a different byte allowance.
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.MaxSourceBytes >= 0 && p.snapshot.Bytes() > s.MaxSourceBytes {
		return ErrByteLimitExceeded
	}
	if s.MaxChangeBytes >= 0 && p.delta.Bytes > s.MaxChangeBytes {
		return ErrByteLimitExceeded
	}
	return nil
}
