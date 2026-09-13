package changes

import (
	"context"
	"fmt"
	"slices"

	"github.com/lydakis/errand/internal/proto"
)

// PreparedTransferSource owns validated source metadata and its canonical
// delta. It contains no live destination values or body-verification evidence.
// Its immutable metadata can cross an upload interval; StagePrepared rechecks
// the receiver checkpoint after recovery and still verifies supplied bodies.
type PreparedTransferSource struct {
	manifest   proto.Manifest
	delta      proto.ChangeBundle
	sourceRoot string
	valid      bool
}

// Manifest and Delta return copies so callers cannot change the prepared plan.
func (p PreparedTransferSource) Manifest() proto.Manifest  { return cloneSourceManifest(p.manifest) }
func (p PreparedTransferSource) Delta() proto.ChangeBundle { return cloneSourceDelta(p.delta) }

func PrepareTransferSource(ctx context.Context, base, current proto.Manifest, maxBytes int64) (PreparedTransferSource, error) {
	if err := validateCheckpointManifestContext(ctx, current); err != nil {
		return PreparedTransferSource{}, err
	}
	delta, err := workspaceDelta(ctx, base, current, maxBytes)
	if err != nil {
		return PreparedTransferSource{}, err
	}
	if err := ValidateBundleContext(ctx, delta); err != nil {
		return PreparedTransferSource{}, err
	}
	return ownPreparedTransferSource(ctx, current, delta, current.RootHash())
}

func ExpandTransferSource(ctx context.Context, base proto.Manifest, delta proto.ChangeBundle, sourceRoot string, maxBytes int64) (PreparedTransferSource, error) {
	current, err := ExpandSourceDeltaContext(ctx, base, delta, sourceRoot, maxBytes)
	if err != nil {
		return PreparedTransferSource{}, err
	}
	return ownPreparedTransferSource(ctx, current, delta, sourceRoot)
}

func ownPreparedTransferSource(ctx context.Context, current proto.Manifest, delta proto.ChangeBundle, sourceRoot string) (PreparedTransferSource, error) {
	if err := ctx.Err(); err != nil {
		return PreparedTransferSource{}, err
	}
	// Both factories have validated the full source and canonical delta.
	// Expansion has also verified its supplied source digest, so retain that
	// evidence instead of hashing or validating the full manifest again.
	p := PreparedTransferSource{manifest: cloneSourceManifest(current), delta: cloneSourceDelta(delta), sourceRoot: sourceRoot, valid: true}
	return p, ctx.Err()
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

func (s TransferSession) StagePrepared(ctx context.Context, id, source string, p PreparedTransferSource) (string, proto.ChangeBundle, error) {
	if !p.valid {
		return "", proto.ChangeBundle{}, fmt.Errorf("transfer source was not prepared")
	}

	return s.stage(ctx, id, source, p.manifest, &p)
}

func (p PreparedTransferSource) validateStageLimits(ctx context.Context, s TransferSession) error {
	// Source quotas count the complete reconstructed tree, including unchanged
	// cached files. Delta factories may have used a different byte allowance.
	var size int64
	for _, e := range p.manifest.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Type == proto.EntryFile {
			if s.MaxSourceBytes >= 0 && e.Size > s.MaxSourceBytes-size {
				return ErrByteLimitExceeded
			}
			size += e.Size
		}
	}
	if s.MaxChangeBytes >= 0 && p.delta.Bytes > s.MaxChangeBytes {
		return ErrByteLimitExceeded
	}
	return nil
}
