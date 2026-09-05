package snapshot

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

// Equal payload sizes distinguish per-file overhead from byte throughput.
// Fixture creation is untimed; these measure a warm local filesystem.
func BenchmarkSnapshotPreparation(b *testing.B) {
	for _, shape := range []struct {
		name         string
		files, bytes int
	}{
		{"128x64KiB", 128, 64 << 10},
		{"4096x2KiB", 4096, 2 << 10},
	} {
		b.Run(shape.name, func(b *testing.B) {
			root := b.TempDir()
			content := make([]byte, shape.bytes)
			for i := 0; i < shape.files; i++ {
				binary.LittleEndian.PutUint64(content, uint64(i))
				path := filepath.Join(root, fmt.Sprintf("d%03d", i/128), fmt.Sprintf("f%05d", i))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					b.Fatal(err)
				}
				if err := os.WriteFile(path, content, 0o644); err != nil {
					b.Fatal(err)
				}
			}
			paths, _, _, err := SelectFilesWithOptions(root, SelectOptions{IncludeAll: true})
			if err != nil {
				b.Fatal(err)
			}
			manifest, err := Build(root, paths)
			if err != nil {
				b.Fatal(err)
			}
			for _, phase := range []struct {
				name string
				run  func() error
			}{
				{"select", func() error {
					_, _, _, err := SelectFilesWithOptions(root, SelectOptions{IncludeAll: true})
					return err
				}},
				{"hash", func() error { _, err := Build(root, paths); return err }},
				{"pack-full", func() error { return Pack(io.Discard, root, manifest) }},
				{"pack-cached", func() error {
					return PackPartial(io.Discard, root, manifest, func(proto.ManifestEntry) bool { return false })
				}},
			} {
				b.Run(phase.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						if err := phase.run(); err != nil {
							b.Fatal(err)
						}
					}
					b.ReportMetric(float64(shape.files), "files/op")
				})
			}
		})
	}
}
