//go:build darwin || linux

package snapshotcheckpoint

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/snapshot"
)

// Fixture construction and verification stay outside the timed comparison.
func BenchmarkVerifiedDifferences(b *testing.B) {
	ctx := context.Background()
	root := b.TempDir()
	paths := make([]string, 10000)
	for i := range paths {
		paths[i] = fmt.Sprintf("file-%05d", i)
		if err := os.WriteFile(filepath.Join(root, paths[i]), nil, 0600); err != nil {
			b.Fatal(err)
		}
	}
	built, err := snapshot.BuildObservedContext(ctx, root, snapshot.PrepareBuildPaths(paths), snapshot.ObservationOptions{Collect: true, MaxBytes: -1, MaxEntries: -1})
	if err != nil {
		b.Fatal(err)
	}
	after, err := built.Verify(ctx)
	if err != nil {
		b.Fatal(err)
	}
	for _, changed := range []bool{false, true} {
		before := make([]observation, after.Len())
		for i := range before {
			before[i] = after.At(i)
		}
		want := 0
		if changed {
			before[len(before)/2].Entry.Mode ^= 0100
			want = 1
		}
		b.Run(fmt.Sprintf("changed=%t", changed), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				edits := differences(before, after)
				if len(edits) != want || changed && (edits[0].Delete || edits[0].Entry != after.At(len(before)/2).Entry) {
					b.Fatal("incorrect difference")
				}
			}
		})
	}
}
