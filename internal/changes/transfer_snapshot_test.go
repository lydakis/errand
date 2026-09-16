package changes

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/snapshot"
)

func TestCopyTransferSourcePreservesMutationError(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "value")
	if err := os.WriteFile(name, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := snapshot.Build(root, []string{"value"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	err = CopyTransferSource(context.Background(), root, filepath.Join(t.TempDir(), "copy"), m, 100)
	if !snapshot.IsSourceChanged(err) {
		t.Fatalf("copy lost source mutation classification: %v", err)
	}
}
