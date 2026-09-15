package changes

import (
	"context"
	"os"

	"github.com/lydakis/errand/internal/archive"
	"github.com/lydakis/errand/internal/proto"
)

// Merge inputs are private scratch, discarded if apply stops. They need verified
// independent bodies, but do not publish a durable tree: the existing apply
// journal synchronizes the chosen output before installation. Keep that policy
// while sharing the same source checks and copier as durable transfer staging.
func materializeMergeInput(source, destination string, m proto.Manifest) error {
	if err := archive.Validate(m); err != nil {
		return err
	}
	tree, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer tree.Close()
	return materializeSourceTree(context.Background(), source, tree, m, mergeInputPermissions,
		func(*os.File) error { return nil }, func() error { return nil })
}
