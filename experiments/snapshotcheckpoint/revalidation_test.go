//go:build darwin || linux

package snapshotcheckpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestRelativeAliasReusesCanonicalCheckpoint(t *testing.T) {
	root, cache := fixture(t)
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	first := oracle(t, root, cache)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	t.Chdir(alias)
	t.Setenv("PWD", alias)
	got, err := Prepare(context.Background(), ".", cache, snapshot.SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.CacheStatus != "hit" || got.Hashed != 0 || got.Written {
		t.Fatalf("relative alias missed canonical checkpoint: %+v", got)
	}
	a, err := first.State.RootHash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := got.State.RootHash(context.Background())
	if err != nil || a != b {
		t.Fatalf("relative alias changed snapshot: %v", err)
	}
}

func TestEmptySelectionKeepsColdWireIdentity(t *testing.T) {
	root, cache := fixture(t)
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), []byte("*\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first := oracle(t, root, cache)
	if first.State.Len() != 0 {
		t.Fatal("fixture was not empty")
	}
	if next := oracle(t, root, cache); next.State.Len() != 0 || next.Written {
		t.Fatalf("empty restart changed state: %+v", next)
	}
}

func TestPreparationRevalidatesRelevantChanges(t *testing.T) {
	for _, name := range []string{"ignored-on-miss", "ignored-on-hit", "selected-sibling", "directory-mode", "directory-replacement", "file-edit"} {
		t.Run(name, func(t *testing.T) {
			root, cache := t.TempDir(), t.TempDir()
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			dir := filepath.Join(root, "dir")
			file := filepath.Join(dir, "a")
			must(os.Mkdir(dir, 0700))
			must(os.WriteFile(filepath.Join(root, ".errandignore"), []byte("dir/.ignored\n"), 0600))
			must(os.WriteFile(file, []byte("before"), 0600))
			if name != "ignored-on-miss" {
				oracle(t, root, cache)
			}
			must(os.WriteFile(file, []byte("after!"), 0600))
			// Use the real builder, then mutate the filesystem at its verification
			// boundary. The per-call seam avoids timing races and global hooks.
			build := func(ctx context.Context, root string, paths []string, bytes int64, entries int) (proto.Manifest, error) {
				part, err := snapshot.BuildBoundedContext(ctx, root, paths, bytes, entries)
				if err != nil {
					return part, err
				}
				switch name {
				case "ignored-on-miss", "ignored-on-hit":
					must(os.WriteFile(filepath.Join(dir, ".ignored"), []byte("build output"), 0600))
				case "selected-sibling":
					must(os.WriteFile(filepath.Join(dir, "new"), []byte("selected"), 0600))
				case "directory-mode":
					must(os.Chmod(dir, 0755))
				case "directory-replacement":
					must(os.Rename(dir, filepath.Join(t.TempDir(), "old")))
					must(os.Mkdir(dir, 0700))
					must(os.WriteFile(file, []byte("after!"), 0600))
				case "file-edit":
					must(os.WriteFile(file, []byte("again!"), 0600))
				}
				return part, nil
			}
			got, err := prepare(context.Background(), root, cache, snapshot.SelectOptions{}, build)
			if name != "ignored-on-miss" && name != "ignored-on-hit" {
				if err == nil {
					t.Fatal("accepted a relevant source change")
				}
				return
			}
			must(err)
			cold, err := Cold(context.Background(), root, snapshot.SelectOptions{})
			must(err)
			a, err := got.State.RootHash(context.Background())
			must(err)
			b, err := cold.State.RootHash(context.Background())
			must(err)
			if a != b {
				t.Fatal("ignored churn changed the selected snapshot")
			}
			if next := oracle(t, root, cache); next.Hashed != 0 || next.Written {
				t.Fatalf("published stale observations: %+v", next)
			}
		})
	}
}
