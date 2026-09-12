package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/snapshot"
)

// Isolate cache population from extraction, source retention, and filesystem
// syncs. The fixture has 1,000 1-KiB files, with every fifth body identical.
func BenchmarkSnapshotCacheIngestion(b *testing.B) {
	root := b.TempDir()
	var paths []string
	for i := 0; i < 1000; i++ {
		path := fmt.Sprintf("file-%04d", i)
		content := fmt.Sprintf("file %08d\n", i)
		if i%5 == 0 {
			content = "shared contents\n"
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(content+strings.Repeat("x", 1024-len(content))), 0600); err != nil {
			b.Fatal(err)
		}
		paths = append(paths, path)
	}
	manifest, err := snapshot.Build(root, paths)
	if err != nil {
		b.Fatal(err)
	}
	for _, state := range []string{"cold", "warm"} {
		b.Run(state, func(b *testing.B) {
			b.SetBytes(1000 * 1024)
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				cache, err := newBlobCache(b.TempDir(), 32<<20, time.Hour)
				if err != nil {
					b.Fatal(err)
				}
				d := &Daemon{cache: cache}
				if state == "warm" {
					if err := d.cacheSnapshotSource(context.Background(), root, manifest, nil); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()
				if err := d.cacheSnapshotSource(context.Background(), root, manifest, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
