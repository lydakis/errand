package changes

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
		deep        bool
		wide        bool
	}{
		{name: "1-files", files: 1}, {name: "512-files", files: 512},
		{name: "512-directories", files: 512, directories: true},
		{name: "32-deep", files: 512, deep: true},
		{name: "8-deep-wide", files: 128, wide: true},
	} {
		b.Run(shape.name, func(b *testing.B) {
			files := shape.files
			root, jobs := b.TempDir(), b.TempDir()
			data := make([]byte, 8*1024*1024/files)
			for i := range data {
				data[i] = byte(i)
			}
			var paths []string
			if shape.deep {
				for depth := 1; depth <= 32; depth++ {
					directory := strings.TrimSuffix(strings.Repeat("d/", depth), "/")
					if err := os.Mkdir(filepath.Join(root, directory), 0700); err != nil {
						b.Fatal(err)
					}
					paths = append(paths, directory)
				}
			}
			for i := 0; i < files; i++ {
				name := fmt.Sprintf("file-%04d", i)
				if shape.deep {
					name = strings.Repeat("d/", i%32+1) + name
				}
				if shape.wide {
					for depth := 1; depth <= 8; depth++ {
						directory := fmt.Sprintf("branch-%04d", i) + strings.Repeat("/d", depth-1)
						if err := os.Mkdir(filepath.Join(root, directory), 0700); err != nil {
							b.Fatal(err)
						}
						paths = append(paths, directory)
					}
					name = fmt.Sprintf("branch-%04d/", i) + strings.Repeat("d/", 7) + name
				}
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
			// Flush fixture writes before measuring capture. Retain captured trees
			// until the entire sample ends: StopTimer alone does not prevent a
			// deletion's deferred writes from joining the next capture's fsync.
			syncCaptureFixture(b, root)
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				job, err := os.MkdirTemp(jobs, "capture-")
				if err != nil {
					b.Fatal(err)
				}
				if err := syncDirectory(jobs); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if err := CaptureWorkspaceBaseContext(context.Background(), root, job, manifest); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func syncCaptureFixture(b *testing.B, root string) {
	b.Helper()
	var directories []string
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, name)
			return nil
		}
		file, err := os.Open(name)
		if err != nil {
			return err
		}
		syncErr := syncStagedData(file)
		closeErr := file.Close()
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	})
	if err != nil {
		b.Fatal(err)
	}
	for i := len(directories) - 1; i >= 0; i-- {
		// One full barrier at the root is sufficient on Darwin after each
		// directory has received the same member fsync used by production.
		dir, err := os.Open(directories[i])
		if err != nil {
			b.Fatal(err)
		}
		syncErr := syncStagedData(dir)
		closeErr := dir.Close()
		if syncErr != nil || closeErr != nil {
			b.Fatalf("sync fixture directory: %v; close: %v", syncErr, closeErr)
		}
	}
	if err := syncDirectory(root); err != nil {
		b.Fatal(err)
	}
}
