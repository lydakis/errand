package changes

import (
	"fmt"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

// Uses only the pre-existing ValidateBundle API so the same benchmark can run
// against the baseline. Fixture construction is outside the measured loop.
func BenchmarkBundleValidationManyRoots(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			bundle := proto.ChangeBundle{V: BundleVersion, BaselineRoot: fmt.Sprintf("%064x", 1)}
			for i := 0; i < count; i++ {
				name := fmt.Sprintf("file-%05d", i)
				before := proto.ManifestEntry{Path: name, Type: proto.EntrySymlink, Mode: 0777, Target: "before"}
				after := before
				after.Target = "after"
				bundle.Paths = append(bundle.Paths, name)
				bundle.BaseManifest.Entries = append(bundle.BaseManifest.Entries, before)
				bundle.RemoteManifest.Entries = append(bundle.RemoteManifest.Entries, after)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := ValidateBundle(bundle); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkAcceptedSource(b *testing.B) {
	var base proto.Manifest
	for i := range 10000 {
		base.Entries = append(base.Entries, proto.ManifestEntry{Path: fmt.Sprintf("file-%05d", i), Type: proto.EntrySymlink, Mode: 0777, Target: "before"})
	}
	current := cloneSourceManifest(base)
	current.Entries[0].Target = "after"
	delta, err := PrepareSourceDelta(b.Context(), base, current, -1)
	if err != nil {
		b.Fatal(err)
	}
	for _, reuse := range []bool{false, true} {
		b.Run(fmt.Sprintf("reuse=%t", reuse), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var err error
				if reuse {
					_, err = AcceptedSourceDelta(b.Context(), base, delta, delta.Paths)
				} else {
					_, err = AcceptedSource(base, current, delta.Paths)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
