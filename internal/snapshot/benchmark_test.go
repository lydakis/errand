package snapshot

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

// Include ignored dependencies: enumerating policy files inside them used to
// dominate Git selection even though none of their contents were shipped.
func BenchmarkGitSelection(b *testing.B) {
	b.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	b.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	b.Setenv("XDG_CONFIG_HOME", b.TempDir())
	root := b.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		b.Fatalf("git init: %v: %s", err, out)
	}
	write := func(name string, content []byte) {
		b.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			b.Fatal(err)
		}
	}
	write(".gitignore", []byte("dependencies/\n"))
	wantPaths := []string{".gitignore"}
	for i := range 128 {
		name := fmt.Sprintf("src/file-%03d", i)
		write(name, []byte("source"))
		wantPaths = append(wantPaths, name)
		write(fmt.Sprintf("dependencies/pkg-%03d/.gitignore", i), []byte("build/\n"))
		for j := range 16 {
			write(fmt.Sprintf("dependencies/pkg-%03d/file-%02d", i, j), []byte("dependency"))
		}
	}
	paths, _, _, err := SelectFiles(root)
	if err != nil {
		b.Fatal(err)
	}
	if !slices.Equal(paths, wantPaths) {
		b.Fatalf("selected paths = %v, want %v", paths, wantPaths)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, _, _, err := SelectFiles(root); err != nil {
			b.Fatal(err)
		}
	}
}

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
