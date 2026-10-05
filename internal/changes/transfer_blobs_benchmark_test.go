package changes

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/snapshot"
)

// Each operation retains one new body, as a one-file watch save does, into a
// store that already holds a body per earlier save until gc changes prunes.
func BenchmarkTransferBlobRetain(b *testing.B) {
	for _, stored := range []int{1000, 8000, 16000} {
		b.Run(fmt.Sprint(stored), func(b *testing.B) {
			store := TransferBlobStore{Directory: b.TempDir(), MaxBytes: 1 << 40}
			for i := range stored {
				body := fmt.Appendf(nil, "stored %08d\n%01000d", i, 0)
				sum := sha256.Sum256(body)
				if err := os.WriteFile(filepath.Join(store.Directory, hex.EncodeToString(sum[:])), body, 0600); err != nil {
					b.Fatal(err)
				}
			}
			source := b.TempDir()
			save := 0
			retain := func() {
				b.StopTimer()
				save++
				if err := os.WriteFile(filepath.Join(source, "edit.txt"), fmt.Appendf(nil, "save %08d\n%01000d", save, 0), 0600); err != nil {
					b.Fatal(err)
				}
				m, err := snapshot.Build(source, []string{"edit.txt"})
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if err := store.Retain(b.Context(), source, m); err != nil {
					b.Fatal(err)
				}
			}
			// The first retention after a prune or crash scans; measure the rest.
			retain()
			b.ResetTimer()
			for b.Loop() {
				retain()
			}
		})
	}
}
