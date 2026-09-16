//go:build !darwin && !linux

package snapshot

import (
	"fmt"
	"io/fs"
)

func changeStamp(fs.FileInfo) (int64, int64, bool) { return 0, 0, false }

func Fingerprint(fs.FileInfo) (ObservationStamp, error) {
	return ObservationStamp{}, fmt.Errorf("unsupported stat evidence")
}
