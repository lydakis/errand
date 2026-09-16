//go:build darwin || linux

package snapshotcheckpoint

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/snapshot"
)

func TestStrategiesPreserveRestartAndStructuralIdentity(t *testing.T) {
	ctx := context.Background()
	root, updateCache := fixture(t)
	currentCache := t.TempDir()
	check := func() {
		t.Helper()
		cold, err := Cold(ctx, root, snapshot.SelectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		want, err := cold.State.RootHash(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, strategy := range []struct {
			name, cache string
			prepare     func(context.Context, string, string, snapshot.SelectOptions) (Result, error)
		}{
			{"update", updateCache, Prepare}, {"current", currentCache, PrepareCurrent},
		} {
			got, err := strategy.prepare(ctx, root, strategy.cache, snapshot.SelectOptions{})
			if err != nil {
				t.Fatalf("%s: %v", strategy.name, err)
			}
			hash, err := got.State.RootHash(ctx)
			if err != nil || hash != want {
				t.Fatalf("%s root differs: %v", strategy.name, err)
			}
		}
	}
	check()
	check()
	// Cross the existing adaptive index threshold, then replace a file with a
	// directory and remove a populated subtree. Compare every transition to cold.
	dir := filepath.Join(root, "wide")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := range 4100 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%04d", i)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	check()
	check()
	if err := os.Remove(filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "a"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../b", filepath.Join(root, "a/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0000"), []byte("y"), 0600); err != nil {
		t.Fatal(err)
	}
	check()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	check()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), []byte("*\n"), 0600); err != nil {
		t.Fatal(err)
	}
	check()
	check()
}
