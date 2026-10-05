package changes

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

// sharedCopies keeps a copy of each body whose path is in paths, like a
// pinned cache would, and names those copies by hash.
func sharedCopies(t *testing.T, root string, m proto.Manifest, paths ...string) SharedBlobs {
	t.Helper()
	store := t.TempDir()
	held := make(map[string]string)
	for _, e := range m.Entries {
		if e.Type != proto.EntryFile || !slices.Contains(paths, e.Path) {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(e.Path)))
		if err != nil {
			t.Fatal(err)
		}
		held[e.SHA256] = filepath.Join(store, e.SHA256)
		if err := os.WriteFile(held[e.SHA256], body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return func(sha string) (string, bool) {
		path, ok := held[sha]
		return path, ok
	}
}

func jobBaseFixture(t *testing.T) (string, proto.Manifest) {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{
		"cached.txt": "cached body\n", "copied.txt": "copied body\n",
		"dir/twin-a.txt": "twin body\n", "dir/twin-b.txt": "twin body\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("copied.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	m, err := snapshot.Build(root, []string{"cached.txt", "copied.txt", "dir", "dir/twin-a.txt", "dir/twin-b.txt", "link"})
	if err != nil {
		t.Fatal(err)
	}
	return root, m
}

func TestJobBaseCopiesOnlyBodiesTheSharedStoreLacks(t *testing.T) {
	workspace, baseline := jobBaseFixture(t)
	job := t.TempDir()
	shared := sharedCopies(t, workspace, baseline, "cached.txt")
	if err := CaptureJobBaseContext(context.Background(), workspace, job, baseline, shared); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadDir(workspaceBasePath(job))
	if err != nil {
		t.Fatal(err)
	}
	// One copy per distinct unshared body: copied.txt and the twins' body.
	if len(stored) != 2 {
		t.Fatalf("private store holds %d bodies, want 2", len(stored))
	}

	// Every file changes, so collection reads every base body from one store
	// or the other.
	for _, name := range []string{"cached.txt", "copied.txt", "dir/twin-a.txt"} {
		if err := os.WriteFile(filepath.Join(workspace, filepath.FromSlash(name)), []byte("job output\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(workspace, "dir/twin-b.txt")); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := CollectWorkspaceChangesContext(context.Background(), workspace, job, shared, baseline, proto.SelectionPolicy{}, 1<<20)
	if err != nil || !collected {
		t.Fatalf("collect = %v, %v", collected, err)
	}
	base := filepath.Join(extractTestBundle(t, job, bundle), "base")
	for name, want := range map[string]string{
		"cached.txt": "cached body\n", "copied.txt": "copied body\n",
		"dir/twin-a.txt": "twin body\n", "dir/twin-b.txt": "twin body\n",
	} {
		got, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Fatalf("base %s = %q, %v; want %q", name, got, err, want)
		}
	}
}

func TestJobBaseWithEveryBodySharedCopiesNothing(t *testing.T) {
	workspace, baseline := jobBaseFixture(t)
	job := t.TempDir()
	shared := sharedCopies(t, workspace, baseline, "cached.txt", "copied.txt", "dir/twin-a.txt")
	if err := CaptureJobBaseContext(context.Background(), workspace, job, baseline, shared); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadDir(workspaceBasePath(job))
	if err != nil || len(stored) != 0 {
		t.Fatalf("private store = %v, %v; want empty", stored, err)
	}
}

func TestJobBaseRejectsAChangedBodyAndLeavesNoStore(t *testing.T) {
	workspace, baseline := jobBaseFixture(t)
	job := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "copied.txt"), []byte("COPIED BODY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CaptureJobBaseContext(context.Background(), workspace, job, baseline, nil); err == nil {
		t.Fatal("captured a body that no longer matches the manifest")
	}
	if _, err := os.Stat(workspaceBasePath(job)); !os.IsNotExist(err) {
		t.Fatalf("failed capture left its store: %v", err)
	}
}

func TestJobBaseKeepsReadOnlyAndSearchOnlySources(t *testing.T) {
	workspace, baseline := jobBaseFixture(t)
	if err := os.Chmod(filepath.Join(workspace, "copied.txt"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(workspace, "dir"), 0o100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(workspace, "dir"), 0o700) })
	baseline, err := snapshot.Build(workspace, []string{"cached.txt", "copied.txt", "dir", "dir/twin-a.txt", "dir/twin-b.txt", "link"})
	if err != nil {
		t.Fatal(err)
	}
	job := t.TempDir()
	if err := CaptureJobBaseContext(context.Background(), workspace, job, baseline, nil); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(workspace, "dir")); err != nil || info.Mode().Perm() != 0o100 {
		t.Fatalf("source directory mode changed: %v, %v", info, err)
	}
	stored, err := os.ReadDir(workspaceBasePath(job))
	if err != nil || len(stored) != 3 {
		t.Fatalf("private store = %v, %v; want 3 bodies", stored, err)
	}
}
