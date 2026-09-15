//go:build !darwin && !linux

package changes

import (
	"github.com/lydakis/errand/internal/proto"
	"os"
)

func openSearchSourceFile(*os.Root, string) (*os.File, error) { return nil, os.ErrPermission }
func openSearchMaterializedDirectory(*os.Root, string) (*os.File, error) {
	return nil, os.ErrPermission
}
func checkSearchSource(*os.Root, proto.ManifestEntry, uint32) error { return os.ErrPermission }
