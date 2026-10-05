//go:build !darwin

package changes

import (
	"os"

	"github.com/lydakis/errand/internal/durable"
)

// Staged members include directories, which Windows can't flush.
func syncStagedData(file *os.File) error {
	return durable.Sync(file)
}
