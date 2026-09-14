package changes

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
)

// Unlike the mostly-warm read benchmark, these cases create a fresh request
// handle each iteration. Fixture creation/durable initialization is untimed.
func BenchmarkCheckpointLifecycle(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		for _, phase := range []string{"cold-read", "initialize-existing"} {
			b.Run(fmt.Sprintf("%d/%s", count, phase), func(b *testing.B) {
				root, state := b.TempDir(), b.TempDir()
				id, _, err := fsidentity.Lstat(root)
				if err != nil {
					b.Fatal(err)
				}
				m := proto.Manifest{}
				for i := range count {
					m.Entries = append(m.Entries, proto.ManifestEntry{Path: fmt.Sprintf("file-%06d", i), Type: proto.EntryFile, Mode: 0600, Size: 1, SHA256: fmt.Sprintf("%064x", i)})
				}
				c := TransferCheckpoint{Root: root, RootID: id, Owner: "owner", SourceID: "source", StatePath: filepath.Join(state, "checkpoint.json")}
				if _, err := c.Initialize(m); err != nil {
					b.Fatal(err)
				}
				b.ResetTimer()
				for b.Loop() {
					s := TransferSession{Directory: state, Root: root, RootID: id, Owner: "owner", SourceID: "source"}
					if phase == "cold-read" {
						v, err := s.Checkpoint().Read()
						if err != nil || len(v.Manifest.Entries) != count {
							b.Fatalf("read: %v", err)
						}
					} else if err := s.Initialize(context.Background(), "unused-existing-source", m); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
