//go:build windows

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

// A corrupt blob is still open for reading when its copy fails verification;
// Windows cannot delete it until that handle is closed.
func TestCorruptBlobIsDeletedOnWindows(t *testing.T) {
	c := testCache(t, 1<<20, time.Hour)
	sha, size := insertContent(t, c, "pristine content")
	if err := os.WriteFile(c.path(sha), []byte("corrupted conten"), 0o600); err != nil {
		t.Fatal(err)
	}
	hit, err := c.Materialize(context.Background(), filepath.Join(t.TempDir(), "f"), proto.ManifestEntry{
		Path: "f", Type: proto.EntryFile, Mode: 0o644, Size: size, SHA256: sha,
	}, nil)
	if err != nil || hit {
		t.Fatalf("corrupt blob materialized: hit=%v err=%v", hit, err)
	}
	if _, err := os.Lstat(c.path(sha)); !os.IsNotExist(err) {
		t.Fatalf("corrupt blob was not deleted: %v", err)
	}
}
