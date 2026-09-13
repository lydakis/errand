package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Manual invalidation isolates preparation from the native event backend. The
// command benchmarks separately cover event delivery and the full transfer.
func BenchmarkWatchPreparation(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		for _, mode := range []string{"first-edit", "retained-edit", "reconcile"} {
			b.Run(fmt.Sprintf("%d/%s", count, mode), func(b *testing.B) {
				root := b.TempDir()
				if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0600); err != nil {
					b.Fatal(err)
				}
				for i := range count {
					if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%05d", i)), []byte("before"), 0600); err != nil {
						b.Fatal(err)
					}
				}
				info, err := os.Lstat(root)
				if err != nil {
					b.Fatal(err)
				}
				w := &Watch{root: root, identity: info, Changed: make(chan struct{}, 1)}
				builder := new(Builder)
				prepare := func() {
					_, _, _, guard, err := w.Prepare(builder)
					if err != nil {
						b.Fatal(err)
					}
					if err := guard.Verify(); err != nil {
						b.Fatal(err)
					}
				}
				prepare()
				name := filepath.Join(root, "file-00000")
				w.invalidatePath(name, false)
				prepare()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					if mode == "first-edit" {
						w.InvalidatePreparation()
						prepare()
					}
					if err := os.WriteFile(name, []byte(fmt.Sprintf("edit-%d", i)), 0600); err != nil {
						b.Fatal(err)
					}
					if mode == "reconcile" {
						w.InvalidatePreparation()
					} else {
						w.invalidatePath(name, false)
					}
					b.StartTimer()
					prepare()
				}
			})
		}
	}
}
