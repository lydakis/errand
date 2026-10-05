package changes

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

// BenchmarkExpandTransferSource expands a one-file delta against a 10K-entry
// checkpoint, as the daemon does for every watch save. "hashed" reuses the
// base's identity; "fresh" computes it, as a newly read checkpoint would.
func BenchmarkExpandTransferSource(b *testing.B) {
	var base proto.Manifest
	for i := range 10000 {
		dir := fmt.Sprintf("packages/pkg-%03d/src", i/100)
		if i%100 == 0 {
			for _, d := range []string{"packages", fmt.Sprintf("packages/pkg-%03d", i/100), dir} {
				if !slices.ContainsFunc(base.Entries, func(e proto.ManifestEntry) bool { return e.Path == d }) {
					base.Entries = append(base.Entries, proto.ManifestEntry{Path: d, Type: proto.EntryDir, Mode: 0755})
				}
			}
		}
		base.Entries = append(base.Entries, proto.ManifestEntry{Path: fmt.Sprintf("%s/file-%05d", dir, i), Type: proto.EntryFile,
			Mode: 0644, Size: 1024, SHA256: fmt.Sprintf("%064x", i)})
	}
	slices.SortFunc(base.Entries, func(x, y proto.ManifestEntry) int { return strings.Compare(x.Path, y.Path) })
	current := cloneSourceManifest(base)
	for i := range current.Entries {
		if current.Entries[i].Type == proto.EntryFile {
			current.Entries[i].SHA256 = fmt.Sprintf("%064x", 10001)
			break
		}
	}
	prepared, err := PrepareTransferSource(context.Background(), base, current, 1<<30)
	if err != nil {
		b.Fatal(err)
	}
	delta, root := prepared.Delta(), current.RootHash()
	hashed := NewSourceBase(base)
	hashed.rootHash()
	for _, name := range []string{"hashed", "fresh"} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				source := hashed
				if name == "fresh" {
					source = &SourceBase{manifest: hashed.manifest, validate: func() error { return nil }, rootHash: sync.OnceValue(hashed.manifest.RootHash)}
				}
				if _, err := ExpandTransferSourceBase(context.Background(), source, delta, root, 1<<30); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
