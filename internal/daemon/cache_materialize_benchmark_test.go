package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/proto"
)

// Include the subsequent durability pass: cached and streamed files must not
// differ in their synchronization cost merely because modes were set by path.
func BenchmarkCachedSourceStaging(b *testing.B) {
	ctx := context.Background()
	cache, err := newBlobCache(filepath.Join(b.TempDir(), "cache"), 1<<20, time.Hour)
	if err != nil {
		b.Fatal(err)
	}
	body := make([]byte, 1024)
	hash := sha256.Sum256(body)
	sum := hex.EncodeToString(hash[:])
	source := filepath.Join(b.TempDir(), "body")
	if err := os.WriteFile(source, body, 0600); err != nil {
		b.Fatal(err)
	}
	if err := cache.Insert(ctx, source, sum, int64(len(body))); err != nil {
		b.Fatal(err)
	}
	var manifest proto.Manifest
	for i := 0; i < 1000; i++ {
		manifest.Entries = append(manifest.Entries, proto.ManifestEntry{Path: fmt.Sprintf("file-%04d", i), Type: proto.EntryFile, Mode: 0644, Size: 1024, SHA256: sum})
	}
	parent := b.TempDir()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		dest, err := os.MkdirTemp(parent, "stage-")
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		for _, e := range manifest.Entries {
			hit, err := cache.Materialize(ctx, filepath.Join(dest, e.Path), e)
			if err != nil || !hit {
				b.Fatalf("materialize hit=%v: %v", hit, err)
			}
		}
		if err := changes.SyncTransferSource(dest, manifest); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if err := changes.RemoveTree(dest); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}
