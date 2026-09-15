package changes

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func BenchmarkVerifiedMergeInputs(b *testing.B) {
	// Normalize the committed API once, outside timing, so this exact benchmark
	// can compare the older access-handle return with the error-only candidate.
	var prepare func(string, string, proto.ChangeBundle) error
	switch fn := any(materializeVerifiedMergeInputs).(type) {
	case func(string, string, proto.ChangeBundle) error:
		prepare = fn
	case func(string, string, proto.ChangeBundle) ([]*treeAccess, error):
		prepare = func(source, dest string, bundle proto.ChangeBundle) error {
			accesses, err := fn(source, dest, bundle)
			for _, access := range accesses {
				if closeErr := access.closeWithoutRestore(); err == nil {
					err = closeErr
				}
			}
			return err
		}
	default:
		b.Fatal("unsupported merge-input API")
	}
	for _, tc := range []struct {
		name        string
		files, size int
		restricted  bool
	}{{"small", 1, 256, false}, {"batch", 128, 4096, false}, {"nested", 1024, 256, false},
		{"restricted", 128, 256, true}, {"large", 1, 8 << 20, false}} {
		b.Run(tc.name, func(b *testing.B) {
			source, outputs := b.TempDir(), b.TempDir()
			b.Cleanup(func() { _ = RemoveTree(source); _ = RemoveTree(outputs) })
			var bundle proto.ChangeBundle
			manifests := []*proto.Manifest{&bundle.BaseManifest, &bundle.RemoteManifest}
			for tree, name := range []string{"base", "remote"} {
				root := filepath.Join(source, name)
				if err := os.Mkdir(root, 0700); err != nil {
					b.Fatal(err)
				}
				var paths []string
				for i := range tc.files {
					dir := fmt.Sprintf("dir-%04d", i/32)
					if i%32 == 0 {
						if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
							b.Fatal(err)
						}
						paths = append(paths, dir)
					}
					path := fmt.Sprintf("%s/file-%04d", dir, i)
					if err := os.WriteFile(filepath.Join(root, path), bytes.Repeat([]byte{byte('A' + tree)}, tc.size), 0600); err != nil {
						b.Fatal(err)
					}
					paths = append(paths, path)
				}
				m, err := snapshot.Build(root, paths)
				if err != nil {
					b.Fatal(err)
				}
				if tc.restricted {
					for i := len(m.Entries) - 1; i >= 0; i-- {
						m.Entries[i].Mode = 0
						if err := os.Chmod(filepath.Join(root, m.Entries[i].Path), 0); err != nil {
							b.Fatal(err)
						}
					}
				}
				*manifests[tree] = m
			}
			b.ResetTimer()
			b.StopTimer()
			for i := 0; i < b.N; i++ {
				dest, err := os.MkdirTemp(outputs, "input-")
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				err = prepare(source, dest, bundle)
				b.StopTimer()
				if err != nil {
					b.Fatal(err)
				}
				for tree, name := range []string{"base", "remote"} {
					want := bytes.Repeat([]byte{byte('A' + tree)}, tc.size)
					for _, entry := range manifests[tree].Entries {
						if entry.Type != proto.EntryFile {
							continue
						}
						got, err := os.ReadFile(filepath.Join(dest, name, entry.Path))
						if err != nil || !bytes.Equal(got, want) {
							b.Fatalf("incorrect copied body: %s, %v", entry.Path, err)
						}
					}
				}
				if err := RemoveTree(dest); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
