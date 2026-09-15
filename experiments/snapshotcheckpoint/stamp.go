//go:build darwin || linux

package snapshotcheckpoint

import (
	"io/fs"

	"github.com/lydakis/errand/internal/proto"
)

type stamp struct {
	Device, Inode                                  uint64
	Size                                           int64
	Mode                                           uint32
	ModifiedSec, ModifiedNS, ChangedSec, ChangedNS int64
	BornSec, BornNS                                int64
}

func matchesPrepared(before, after stamp, entry proto.ManifestEntry) bool {
	if entry.Type != proto.EntryDir {
		return before == after
	}
	// Ignored sibling creation changes directory size/timestamps without
	// changing its manifest metadata. SelectionGuard verifies membership.
	// Keep identity and full mode checks, including the builder's observed mode.
	return fs.FileMode(before.Mode).IsDir() && fs.FileMode(after.Mode).IsDir() &&
		before.Device == after.Device && before.Inode == after.Inode &&
		before.BornSec == after.BornSec && before.BornNS == after.BornNS &&
		before.Mode == after.Mode && entry.Mode == uint32(fs.FileMode(after.Mode).Perm())
}
