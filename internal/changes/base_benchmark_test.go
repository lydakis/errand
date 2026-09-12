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
	for _, shape := range []struct {
		name        string
		files       int
		directories bool
	}{
		{"1-files", 1, false}, {"512-files", 512, false}, {"512-directories", 512, true},
	} {
		b.Run(shape.name, func(b *testing.B) {
			files := shape.files
			root, job := b.TempDir(), b.TempDir()
			data := make([]byte, 8*1024*1024/files)
			for i := range data {
				data[i] = byte(i)
			}
			var paths []string
			for i := 0; i < files; i++ {
				name := fmt.Sprintf("file-%04d", i)
				if shape.directories {
					directory := fmt.Sprintf("dir-%04d", i)
					if err := os.Mkdir(filepath.Join(root, directory), 0700); err != nil {
						b.Fatal(err)
					}
					paths = append(paths, directory)
					name = directory + "/" + name
				}
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
