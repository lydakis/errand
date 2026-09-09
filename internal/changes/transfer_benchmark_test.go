package changes

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

// Compare freezing the full source with staging a small delta. This deliberately
// keeps a large unchanged file: source reconstruction cost must remain visible.
func BenchmarkTransferPreparation(b *testing.B) {
	for _, kind := range []string{"freeze-source", "stage-small-delta"} {
		b.Run(kind, func(b *testing.B) {
			ctx := context.Background()
			source := b.TempDir()
			const size = 8 << 20
			for name, body := range map[string][]byte{"bulk": bytes.Repeat([]byte("x"), size), "value": []byte("before\n")} {
				if err := os.WriteFile(filepath.Join(source, name), body, 0600); err != nil {
					b.Fatal(err)
				}
			}
			initial, err := snapshot.Build(source, []string{"bulk", "value"})
			if err != nil {
				b.Fatal(err)
			}
			root := b.TempDir()
			identity, _, err := fsidentity.Lstat(root)
			if err != nil {
				b.Fatal(err)
			}
			session := TransferSession{Directory: b.TempDir(), Root: root, RootID: identity, Owner: "benchmark", SourceID: "source", MaxSourceBytes: 32 << 20, MaxChangeBytes: 1 << 20}
			if err := session.Initialize(ctx, source, initial); err != nil {
				b.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "value"), []byte("after\n"), 0600); err != nil {
				b.Fatal(err)
			}
			current, err := snapshot.Build(source, []string{"bulk", "value"})
			if err != nil {
				b.Fatal(err)
			}
			temp := b.TempDir()
			var retained int64
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var dir string
				if kind == "freeze-source" {
					dir = filepath.Join(temp, "source")
					err = CopyTransferSource(ctx, source, dir, current, session.MaxSourceBytes)
				} else {
					dir, _, err = session.Stage(ctx, proto.NewULID(), source, current)
				}
				if err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				retained, err = TransferStorageBytes(dir)
				if err != nil {
					b.Fatal(err)
				}
				if err := RemoveTree(dir); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
			b.StopTimer()
			b.ReportMetric(float64(retained), "retained-B/op")
		})
	}
}
