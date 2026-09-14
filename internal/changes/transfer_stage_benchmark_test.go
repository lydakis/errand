package changes

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func BenchmarkTransferStageBodies(b *testing.B) {
	for _, tc := range []struct {
		name        string
		files, size int
	}{{"small", 1, 256}, {"batch", 128, 4096}, {"large", 1, 8 << 20}} {
		b.Run(tc.name, func(b *testing.B) {
			base, source, root, state := b.TempDir(), b.TempDir(), b.TempDir(), b.TempDir()
			paths := make([]string, tc.files)
			for i := range tc.files {
				paths[i] = fmt.Sprintf("file-%04d", i)
				for j, dir := range []string{base, source} {
					if err := os.WriteFile(filepath.Join(dir, paths[i]), bytes.Repeat([]byte{byte(j + 65)}, tc.size), 0600); err != nil {
						b.Fatal(err)
					}
				}
			}
			before, err := snapshot.Build(base, paths)
			if err != nil {
				b.Fatal(err)
			}
			after, err := snapshot.Build(source, paths)
			if err != nil {
				b.Fatal(err)
			}
			id, _, err := fsidentity.Lstat(root)
			if err != nil {
				b.Fatal(err)
			}
			s := TransferSession{Directory: state, Root: root, RootID: id, Owner: "owner", SourceID: "sender", MaxSourceBytes: 1 << 30, MaxChangeBytes: 1 << 30}
			if err := s.Initialize(b.Context(), base, before); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for b.Loop() {
				dir, _, err := s.Stage(b.Context(), proto.NewULID(), source, after)
				if err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if _, err := ReadTransferBundle(dir); err != nil {
					b.Fatal(err)
				}
				if err := RemoveTree(dir); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
}
