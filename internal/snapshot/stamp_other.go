//go:build !darwin && !linux

package snapshot

import "io/fs"

func changeStamp(fs.FileInfo) (int64, int64, bool) { return 0, 0, false }
