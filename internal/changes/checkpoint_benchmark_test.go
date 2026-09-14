package changes

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
)

// Keep the record read and destination/storage guards in the timed path.
// Each checkpoint read is a public export, so caller ownership is included.
func BenchmarkCheckpointRead(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			root := b.TempDir()
			id, _, err := fsidentity.Lstat(root)
			if err != nil {
				b.Fatal(err)
			}
			c := TransferCheckpoint{Root: root, RootID: id, Owner: "owner", SourceID: "source", StatePath: filepath.Join(b.TempDir(), "checkpoint.json")}
			m := proto.Manifest{}
			for i := range count {
				m.Entries = append(m.Entries, proto.ManifestEntry{Path: fmt.Sprintf("file-%06d", i), Type: proto.EntryFile, Mode: 0600, Size: 1, SHA256: fmt.Sprintf("%064x", i)})
			}
			if _, err := c.Initialize(m); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for b.Loop() {
				v, err := c.Read()
				if err != nil || len(v.Manifest.Entries) != count {
					b.Fatalf("checkpoint read: %v", err)
				}
			}
		})
	}
}
