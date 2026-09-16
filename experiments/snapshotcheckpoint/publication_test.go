//go:build darwin || linux

package snapshotcheckpoint

import (
	"context"
	"github.com/lydakis/errand/internal/snapshot"
	"os"
	"path/filepath"
	"testing"
)

func TestPublicationRequiresVerifiedBatchForMatchingRoot(t *testing.T) {
	root, cache := fixture(t)
	verified := verifiedFixture(t, root)
	key := identity{Root: verified.Root()}
	if _, err := writeCheckpoint(context.Background(), cache, key, verified); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(cache, "checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		key   identity
		batch snapshot.VerifiedObservations
	}{
		{key, snapshot.VerifiedObservations{}},
		{identity{Root: t.TempDir()}, verified},
	} {
		if _, err := writeCheckpoint(context.Background(), cache, item.key, item.batch); err == nil {
			t.Fatal("unverified publication accepted")
		}
	}
	after, err := os.ReadFile(filepath.Join(cache, "checkpoint"))
	if err != nil || string(before) != string(after) {
		t.Fatal("rejected publication changed generation")
	}
}
