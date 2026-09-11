package changes

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/snapshot"
)

// Keep total bytes fixed to expose the cost of capturing many separate files.
// Set TMPDIR to the job-storage filesystem when measuring disk sync costs;
// a RAM-backed /tmp hides them.
func BenchmarkCaptureWorkspaceBase(b *testing.B) {
	for _, files := range []int{1, 512} {
		b.Run(fmt.Sprintf("%d-files", files), func(b *testing.B) {
			root, job := b.TempDir(), b.TempDir()
			data := make([]byte, 8*1024*1024/files)
			for i := range data {
				data[i] = byte(i)
			}
			var paths []string
			for i := 0; i < files; i++ {
				name := fmt.Sprintf("file-%04d", i)
				if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
					b.Fatal(err)
				}
				paths = append(paths, name)
			}
			manifest, err := snapshot.Build(root, paths)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := CaptureWorkspaceBaseContext(context.Background(), root, job, manifest); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if err := os.RemoveAll(workspaceBasePath(job)); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
}
