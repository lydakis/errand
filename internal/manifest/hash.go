package manifest

import (
	"crypto/sha256"
	"encoding/binary"

	"github.com/lydakis/errand/internal/proto"
)

func entryDigest(entry proto.ManifestEntry) [32]byte {
	// Length-prefixed fields preserve exact boundaries without allocating JSON.
	// Common entries fit on the stack; longer paths/targets grow normally. This
	// is an internal identity encoding, not the protocol's Manifest.RootHash.
	var storage [512]byte
	data := append(storage[:0], 1) // Entry encoding version/domain.
	for _, value := range []string{entry.Path, entry.Type, entry.SHA256, entry.Target} {
		data = binary.LittleEndian.AppendUint64(data, uint64(len(value)))
		data = append(data, value...)
	}
	data = binary.LittleEndian.AppendUint32(data, entry.Mode)
	data = binary.LittleEndian.AppendUint64(data, uint64(entry.Size))
	return sha256.Sum256(data)
}
