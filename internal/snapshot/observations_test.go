//go:build darwin || linux

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestObservedBuilderLimitsAndVerification(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "dir/a")
	if err := os.WriteFile(file, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	paths := PrepareBuildPaths([]string{"dir/a"})
	if paths.Len() != 2 {
		t.Fatal("missing implicit ancestor")
	}
	first, err := BuildObservedContext(ctx, root, paths, ObservationOptions{Collect: true, MaxBytes: -1, MaxEntries: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Verify(ctx, root); err != nil {
		t.Fatal(err)
	}
	if len(first.Observations) != 2 || first.Hashed != 1 {
		t.Fatalf("missing ancestor or body: %+v", first)
	}
	warm, err := BuildObservedContext(ctx, root, paths, ObservationOptions{Prior: first.Observations, Collect: true, MaxBytes: -1, MaxEntries: -1})
	if err != nil || warm.Reused != 1 || warm.Hashed != 0 || warm.Changed {
		t.Fatalf("reuse: %+v %v", warm, err)
	}
	bypass, err := BuildObservedContext(ctx, root, paths, ObservationOptions{Prior: first.Observations, Collect: false, MaxBytes: -1, MaxEntries: -1})
	if err != nil || len(bypass.Observations) != 0 || bypass.Hashed != 1 || bypass.Reused != 0 {
		t.Fatalf("bypass: %+v %v", bypass, err)
	}
	if bypass.Manifest.RootHash() != first.Manifest.RootHash() {
		t.Fatal("bypass changed snapshot")
	}
	for _, prior := range [][]Observation{nil, first.Observations} {
		if _, err := BuildObservedContext(ctx, root, paths, ObservationOptions{Prior: prior, Collect: true, MaxBytes: 5, MaxEntries: -1}); !errors.Is(err, ErrByteLimitExceeded) {
			t.Fatalf("byte limit: %v", err)
		}
		if _, err := BuildObservedContext(ctx, root, paths, ObservationOptions{Prior: prior, Collect: true, MaxBytes: -1, MaxEntries: 1}); !errors.Is(err, ErrEntryLimitExceeded) {
			t.Fatalf("entry limit: %v", err)
		}
	}
	if err := os.WriteFile(file, []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := first.Verify(ctx, root); err == nil {
		t.Fatal("accepted changed body")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := BuildObservedContext(cancelled, root, paths, ObservationOptions{Collect: true, MaxBytes: -1, MaxEntries: -1}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func BenchmarkObservationBypass(b *testing.B) {
	ctx := context.Background()
	root := b.TempDir()
	paths := make([]string, 1000)
	body := make([]byte, 65536)
	for i := range paths {
		paths[i] = fmt.Sprintf("file-%04d", i)
		if err := os.WriteFile(filepath.Join(root, paths[i]), body, 0600); err != nil {
			b.Fatal(err)
		}
	}
	want, err := BuildBoundedContext(ctx, root, paths, -1, -1)
	if err != nil {
		b.Fatal(err)
	}
	for _, name := range []string{"ordinary", "disabled"} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			var got proto.Manifest
			for range b.N {
				if name == "ordinary" {
					got, err = BuildBoundedContext(ctx, root, paths, -1, -1)
				} else {
					var built *ObservedBuild
					built, err = BuildObservedContext(ctx, root, PrepareBuildPaths(paths), ObservationOptions{MaxBytes: -1, MaxEntries: -1})
					if err == nil {
						got = built.Manifest
					}
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if got.RootHash() != want.RootHash() {
				b.Fatal("bypass changed manifest")
			}
		})
	}
}
