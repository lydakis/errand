package snapshot

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// atomicSave replaces name as editors do: write a temporary sibling, then
// rename it over the original. Both names are reported as entry events.
func atomicSave(t *testing.T, w *Watch, name, body string) {
	t.Helper()
	target := filepath.Join(w.root, filepath.FromSlash(name))
	temporary := target + ".save"
	if err := os.WriteFile(temporary, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, target); err != nil {
		t.Fatal(err)
	}
	w.invalidatePath(temporary, dirtyEntry)
	w.invalidatePath(target, dirtyEntry)
}

// assertPreparedMode prepares, checks the result against a fresh snapshot and
// fails unless preparation took the expected full or relisted path.
func assertPreparedMode(t *testing.T, w *Watch, b *Builder, wantFull bool) *SelectionGuard {
	t.Helper()
	before := w.prepared.fullAt
	guard := assertPreparedMatchesFull(t, w, b)
	if full := w.prepared.fullAt != before; full != wantFull {
		t.Fatalf("full selection = %t, want %t", full, wantFull)
	}
	return guard
}

type relistFixture struct {
	name    string
	prepare func(*testing.T) (*Watch, *Builder)
}

var relistFixtures = []relistFixture{
	{"explicit", func(t *testing.T) (*Watch, *Builder) {
		w, b := prepareWatchFixture(t)
		writeFile(t, w.root, "sub/tracked", "tracked")
		return w, b
	}},
	{"git", func(t *testing.T) (*Watch, *Builder) {
		w, b, _ := prepareGitWatchFixture(t)
		return w, b
	}},
}

func TestWatchPrepareRelistsDirectoryForAtomicSaves(t *testing.T) {
	for _, fixture := range relistFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			w, b := fixture.prepare(t)
			assertPreparedMatchesFull(t, w, b)
			for i, name := range []string{"value", "sub/tracked", "value"} {
				atomicSave(t, w, name, string(rune('a'+i)))
				assertPreparedMode(t, w, b, false)
			}
			// The relisted stamps now back the evidence: an in-place edit
			// stays incremental, and later unhinted membership changes in a
			// relisted directory are still detected.
			writeFile(t, w.root, "value", "in place")
			w.invalidatePath(filepath.Join(w.root, "value"), dirtyContent)
			guard := assertPreparedMode(t, w, b, false)
			writeFile(t, w.root, "sub/new", "new")
			if err := guard.Verify(); err == nil {
				t.Fatal("guard accepted an unhinted creation in a relisted directory")
			}
			w.invalidatePath(filepath.Join(w.root, "value"), dirtyContent)
			assertPreparedMode(t, w, b, true)
		})
	}
}

func TestWatchPrepareRelistingRejectsMembershipChanges(t *testing.T) {
	for _, fixture := range relistFixtures {
		for _, tc := range []struct {
			name   string
			change func(t *testing.T, w *Watch)
		}{
			{"created-file", func(t *testing.T, w *Watch) {
				writeFile(t, w.root, "sub/new", "new")
				w.invalidatePath(filepath.Join(w.root, "sub", "new"), dirtyEntry)
			}},
			{"created-ignored-file", func(t *testing.T, w *Watch) {
				writeFile(t, w.root, "sub/secret", "private")
				w.invalidatePath(filepath.Join(w.root, "sub", "secret"), dirtyEntry)
			}},
			{"removed-file", func(t *testing.T, w *Watch) {
				if err := os.Remove(filepath.Join(w.root, "value")); err != nil {
					t.Fatal(err)
				}
				w.invalidatePath(filepath.Join(w.root, "value"), dirtyEntry)
			}},
			{"unhinted-create-beside-save", func(t *testing.T, w *Watch) {
				writeFile(t, w.root, "other", "other")
				atomicSave(t, w, "value", "saved")
			}},
			{"temporary-not-yet-renamed", func(t *testing.T, w *Watch) {
				writeFile(t, w.root, "value.save", "pending")
				w.invalidatePath(filepath.Join(w.root, "value.save"), dirtyEntry)
			}},
			{"replaced-by-directory", func(t *testing.T, w *Watch) {
				if err := os.Remove(filepath.Join(w.root, "value")); err != nil {
					t.Fatal(err)
				}
				writeFile(t, w.root, "value/nested", "nested")
				w.invalidatePath(filepath.Join(w.root, "value"), dirtyEntry)
			}},
			{"replaced-by-symlink", func(t *testing.T, w *Watch) {
				name := filepath.Join(w.root, "value")
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("sub/tracked", name); err != nil {
					t.Fatal(err)
				}
				w.invalidatePath(name, dirtyEntry)
			}},
			{"save-beside-unhinted-directory-change", func(t *testing.T, w *Watch) {
				writeFile(t, w.root, "sub/new", "new")
				atomicSave(t, w, "value", "saved")
			}},
		} {
			t.Run(fixture.name+"/"+tc.name, func(t *testing.T) {
				w, b := fixture.prepare(t)
				assertPreparedMatchesFull(t, w, b)
				tc.change(t, w)
				assertPreparedMode(t, w, b, true)
			})
		}
	}
}

func TestWatchFilesRelistsNativeAtomicSaves(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".errandignore", "ignored/\n")
	writeFile(t, root, "value", "old")
	w, err := WatchFiles(root, SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	b := new(Builder)
	assertPreparedMatchesFull(t, w, b)
	for i := range 3 {
		before := w.Generation()
		writeFile(t, w.root, "value.save", string(rune('a'+i)))
		if err := os.Rename(filepath.Join(w.root, "value.save"), filepath.Join(w.root, "value")); err != nil {
			t.Fatal(err)
		}
		deadline := time.NewTimer(3 * time.Second)
		for w.Generation() == before {
			select {
			case <-w.Changed:
			case err := <-w.Errors:
				t.Fatal(err)
			case <-deadline.C:
				t.Fatal("missed atomic save")
			}
		}
		deadline.Stop()
		// Let the native backend deliver the rest of the save's events.
		time.Sleep(50 * time.Millisecond)
		assertPreparedMode(t, w, b, false)
	}
}
