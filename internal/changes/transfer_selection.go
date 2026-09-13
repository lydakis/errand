package changes

import (
	"errors"
	"fmt"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

var ErrNoTransferChanges = errors.New("no changes in this transfer")

// SelectTransferPaths selects complete change roots for application. A child of
// a replaced directory must be applied using that root, just as with job fetch.
func SelectTransferPaths(b proto.ChangeBundle, requested string) (map[string]bool, error) {
	if requested == "" {
		return nil, nil
	}
	selected := map[string]bool{}
	for _, p := range b.Paths {
		if requested == p || strings.HasPrefix(p, strings.TrimSuffix(requested, "/")+"/") {
			selected[p] = true
		}
		if strings.HasPrefix(requested, p+"/") {
			return nil, fmt.Errorf("path %q is inside retained change root %q; apply the root instead", requested, p)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("path %q has %w", requested, ErrNoTransferChanges)
	}
	return selected, nil
}
