//go:build !darwin && !linux

package outputs

import (
	"fmt"
	"os"
)

func renameNoReplace(_ *os.File, _ string, _ *os.File, _ string) error {
	return fmt.Errorf("atomic output installation is unsupported on this platform")
}
