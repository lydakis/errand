package changes

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
)

// Isolate retained receiver metadata from request-local ownership changes.
// Both modes reread identical record bytes through a fresh request handle.
func BenchmarkCheckpointRequests(b *testing.B) {
	for _, reuse := range []bool{false, true} {
		b.Run(fmt.Sprintf("reuse=%t", reuse), func(b *testing.B) {
			root, state := b.TempDir(), b.TempDir()
			id, _, err := fsidentity.Lstat(root)
			if err != nil {
				b.Fatal(err)
			}
			var cache *CheckpointCache
			if reuse {
				cache = NewCheckpointCache(32, 64<<20)
			}
			m := proto.Manifest{}
			for i := range 10000 {
				m.Entries = append(m.Entries, proto.ManifestEntry{Path: fmt.Sprintf("file-%06d", i), Type: proto.EntryFile, Mode: 0600, Size: 1, SHA256: fmt.Sprintf("%064x", i)})
			}
			c := TransferCheckpoint{Root: root, RootID: id, Owner: "owner", SourceID: "source", StatePath: filepath.Join(state, "checkpoint.json")}
			if _, err := c.Initialize(m); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for b.Loop() {
				s := TransferSession{Directory: state, Root: root, RootID: id, Owner: "owner", SourceID: "source", Reuse: cache}
				v, err := s.Checkpoint().Read()
				if err != nil || len(v.Manifest.Entries) != 10000 {
					b.Fatalf("read: %v", err)
				}
			}
		})
	}
}
