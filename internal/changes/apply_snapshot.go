package changes

import (
	"context"
	"os"

	"github.com/lydakis/errand/internal/proto"
)

// Copy verified inputs directly rather than routing them through tar, which
// carries no Windows directory-link flag. This also avoids a temporary archive
// write and reread. Destination is disposable private merge scratch.
func materializeTypedApplySnapshot(source, destination string, manifest proto.Manifest, strict bool) (*treeAccess, error) {
	tree, err := os.OpenRoot(destination)
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	policy := scratchMaterialization()
	policy.permissions = manifestPermissions
	ctx := context.Background()
	if strict {
		root, openErr := os.OpenRoot(source)
		if openErr != nil {
			return nil, openErr
		}
		defer root.Close()
		err = materializeSourceAtRoot(ctx, root, tree, manifest, nil, policy)
	} else {
		err = materializeSourceTree(ctx, source, tree, manifest, policy)
	}
	if err != nil {
		return nil, err
	}
	return makeTreeAccessible(destination)
}
