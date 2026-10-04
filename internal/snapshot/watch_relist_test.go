package snapshot

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path"
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

func removeFile(t *testing.T, w *Watch, name string, hint bool) {
	t.Helper()
	abs := filepath.Join(w.root, filepath.FromSlash(name))
	if err := os.Remove(abs); err != nil {
		t.Fatal(err)
	}
	if hint {
		w.invalidatePath(abs, dirtyEntry)
	}
}

// assertPreparedMode prepares, checks the result against a fresh snapshot and
// fails unless preparation took the expected full or incremental path.
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
	git     bool
	prepare func(*testing.T) (*Watch, *Builder)
}

var relistFixtures = []relistFixture{
	{"explicit", false, func(t *testing.T) (*Watch, *Builder) {
		w, b := prepareWatchFixture(t)
		writeFile(t, w.root, "sub/tracked", "tracked")
		writeFile(t, w.root, "logs/secret", "private")
		writeFile(t, w.root, "secret", "private")
		return w, b
	}},
	{"git", true, func(t *testing.T) (*Watch, *Builder) {
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
			// stays incremental, and a later unhinted creation in a relisted
			// directory fails the guard and is picked up by relisting.
			writeFile(t, w.root, "value", "in place")
			w.invalidatePath(filepath.Join(w.root, "value"), dirtyContent)
			guard := assertPreparedMode(t, w, b, false)
			writeFile(t, w.root, "sub/new", "new")
			if err := guard.Verify(); err == nil {
				t.Fatal("guard accepted an unhinted creation in a relisted directory")
			}
			w.invalidatePath(filepath.Join(w.root, "value"), dirtyContent)
			assertPreparedMode(t, w, b, false)
		})
	}
}

func TestWatchPrepareRelistingReobservesUnhintedReplacements(t *testing.T) {
	for _, fixture := range relistFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			w, b := fixture.prepare(t)
			assertPreparedMatchesFull(t, w, b)
			// sub/tracked is replaced without an event; saving value relists
			// the root only, so sub is relisted through its changed stamp.
			if err := os.WriteFile(filepath.Join(w.root, "sub", "tracked.save"), []byte("replaced"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(w.root, "sub", "tracked.save"), filepath.Join(w.root, "sub", "tracked")); err != nil {
				t.Fatal(err)
			}
			atomicSave(t, w, "value", "saved")
			assertPreparedMode(t, w, b, false)
		})
	}
}

func TestWatchPrepareNarrowsFileCreationsAndRemovals(t *testing.T) {
	for _, fixture := range relistFixtures {
		for _, tc := range []struct {
			name   string
			git    bool // Git fixture only
			change func(t *testing.T, w *Watch)
		}{
			{"created-file", false, func(t *testing.T, w *Watch) {
				writeFile(t, w.root, "sub/new", "new")
				w.invalidatePath(filepath.Join(w.root, "sub", "new"), dirtyEntry)
			}},
			{"created-file-without-event", false, func(t *testing.T, w *Watch) {
				writeFile(t, w.root, "sub/new", "new")
			}},
			{"created-ignored-file", false, func(t *testing.T, w *Watch) {
				writeFile(t, w.root, "sub/secret", "private")
				w.invalidatePath(filepath.Join(w.root, "sub", "secret"), dirtyEntry)
			}},
			{"removed-file", false, func(t *testing.T, w *Watch) {
				removeFile(t, w, "value", true)
			}},
			{"removed-file-without-event", false, func(t *testing.T, w *Watch) {
				removeFile(t, w, "value", false)
			}},
			{"removed-ignored-file", false, func(t *testing.T, w *Watch) {
				removeFile(t, w, "secret", true)
			}},
			{"temporary-not-yet-renamed", false, func(t *testing.T, w *Watch) {
				writeFile(t, w.root, "value.save", "pending")
				w.invalidatePath(filepath.Join(w.root, "value.save"), dirtyEntry)
			}},
			{"create-beside-save", false, func(t *testing.T, w *Watch) {
				writeFile(t, w.root, "other", "other")
				atomicSave(t, w, "value", "saved")
			}},
			{"selected-file-in-unselected-directory", true, func(t *testing.T, w *Watch) {
				writeFile(t, w.root, "logs/notes.txt", "notes")
				w.invalidatePath(filepath.Join(w.root, "logs", "notes.txt"), dirtyEntry)
			}},
			{"ignored-file-in-unselected-directory", true, func(t *testing.T, w *Watch) {
				writeFile(t, w.root, "logs/b.log", "log")
				w.invalidatePath(filepath.Join(w.root, "logs", "b.log"), dirtyEntry)
			}},
		} {
			if tc.git && !fixture.git {
				continue
			}
			t.Run(fixture.name+"/"+tc.name, func(t *testing.T) {
				w, b := fixture.prepare(t)
				assertPreparedMatchesFull(t, w, b)
				tc.change(t, w)
				assertPreparedMode(t, w, b, false)
			})
		}
	}
}

// TestWatchPrepareDropsHashesOfRelistedRemovals removes distinct files without
// events, so only their directory's changed stamp reports each removal. The
// builder must not keep observations of files no longer selected.
func TestWatchPrepareDropsHashesOfRelistedRemovals(t *testing.T) {
	for _, fixture := range relistFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			w, b := fixture.prepare(t)
			assertPreparedMatchesFull(t, w, b)
			baseline := len(b.hashes)
			for i := range 5 {
				rel := fmt.Sprintf("sub/transient-%d", i)
				abs := filepath.Join(w.root, filepath.FromSlash(rel))
				writeFile(t, w.root, rel, "transient")
				w.invalidatePath(abs, dirtyEntry)
				assertPreparedMode(t, w, b, false)
				if _, ok := b.hashes[abs]; !ok {
					t.Fatalf("created %s was not observed", rel)
				}
				removeFile(t, w, rel, false)
				assertPreparedMode(t, w, b, false)
				if _, ok := b.hashes[abs]; ok {
					t.Fatalf("builder kept the hash of removed %s", rel)
				}
			}
			if len(b.hashes) != baseline {
				t.Fatalf("builder holds %d hashes after relisted removals, want %d", len(b.hashes), baseline)
			}
			w.InvalidatePreparation()
			assertPreparedMode(t, w, b, true)
			if len(b.hashes) != baseline {
				t.Fatalf("full preparation holds %d hashes, want %d", len(b.hashes), baseline)
			}
		})
	}
}

func TestWatchPrepareRelistingFallsBackForStructuralChanges(t *testing.T) {
	for _, fixture := range relistFixtures {
		for _, tc := range []struct {
			name   string
			change func(t *testing.T, w *Watch)
		}{
			{"replaced-by-directory", func(t *testing.T, w *Watch) {
				removeFile(t, w, "value", false)
				writeFile(t, w.root, "value/nested", "nested")
				w.invalidatePath(filepath.Join(w.root, "value"), dirtyEntry)
			}},
			{"replaced-by-symlink", func(t *testing.T, w *Watch) {
				removeFile(t, w, "value", false)
				if err := os.Symlink("sub/tracked", filepath.Join(w.root, "value")); err != nil {
					t.Fatal(err)
				}
				w.invalidatePath(filepath.Join(w.root, "value"), dirtyEntry)
			}},
			{"created-directory", func(t *testing.T, w *Watch) {
				writeFile(t, w.root, "fresh/file", "file")
				w.invalidatePath(filepath.Join(w.root, "fresh", "file"), dirtyEntry)
			}},
			{"created-ignore-file", func(t *testing.T, w *Watch) {
				// Classified as a control event natively; entry-only here.
				writeFile(t, w.root, "sub/.gitignore", "tracked\n")
				writeFile(t, w.root, "sub/.errandignore", "tracked\n")
				w.invalidatePath(filepath.Join(w.root, "sub", ".gitignore"), dirtyEntry)
			}},
			{"removed-directory", func(t *testing.T, w *Watch) {
				if err := os.RemoveAll(filepath.Join(w.root, "sub")); err != nil {
					t.Fatal(err)
				}
				w.invalidatePath(filepath.Join(w.root, "sub"), dirtyEntry)
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

func TestWatchPrepareGitRemovalThatEmptiesDirectoryUsesFullSelection(t *testing.T) {
	w, b, _ := prepareGitWatchFixture(t)
	assertPreparedMatchesFull(t, w, b)
	removeFile(t, w, "sub/tracked", true)
	assertPreparedMode(t, w, b, true)
}

// TestWatchPrepareMatchesFullSelectionUnderRandomChanges applies random
// creations, removals, in-place and rename-over saves, a quarter of them
// without events, and compares every preparation with a fresh selection.
// In-place edits of existing files are always hinted, as the fast path
// requires; rename-over saves change their directory's stamp.
func TestWatchPrepareMatchesFullSelectionUnderRandomChanges(t *testing.T) {
	for _, fixture := range relistFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			w, b := fixture.prepare(t)
			assertPreparedMatchesFull(t, w, b)
			rng := rand.New(rand.NewPCG(7, uint64(len(fixture.name))))
			dirs := []string{".", "sub", "logs"}
			names := []string{"a", "b.log", "secret", "c.txt"}
			incremental := 0
			const steps = 120
			for step := range steps {
				for range 1 + rng.IntN(3) {
					rel := path.Join(dirs[rng.IntN(len(dirs))], names[rng.IntN(len(names))])
					abs := filepath.Join(w.root, filepath.FromSlash(rel))
					_, err := os.Lstat(abs)
					exists := err == nil
					hint := rng.IntN(4) != 0
					body := string(rune('a' + step%26))
					switch op := rng.IntN(3); {
					case exists && op == 0:
						removeFile(t, w, rel, hint)
					case op == 1:
						if err := os.WriteFile(abs+".save", []byte(body), 0600); err != nil {
							t.Fatal(err)
						}
						if err := os.Rename(abs+".save", abs); err != nil {
							t.Fatal(err)
						}
						if hint {
							w.invalidatePath(abs+".save", dirtyEntry)
							w.invalidatePath(abs, dirtyEntry)
						}
					default:
						if err := os.WriteFile(abs, []byte(body), 0600); err != nil {
							t.Fatal(err)
						}
						switch {
						case exists:
							w.invalidatePath(abs, dirtyContent)
						case hint:
							w.invalidatePath(abs, dirtyEntry)
						}
					}
				}
				before := w.prepared.fullAt
				assertPreparedMatchesFull(t, w, b)
				if w.prepared.fullAt == before {
					incremental++
				}
			}
			if incremental < steps/2 {
				t.Fatalf("only %d of %d preparations were incremental", incremental, steps)
			}
		})
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

// createDuringCapture creates the directory dir after selection has
// enumerated the tree and before capture stamps directories.
func createDuringCapture(t *testing.T, w *Watch, dir string) {
	t.Helper()
	t.Cleanup(func() { testHookBeforeDirectoryStamps = nil })
	testHookBeforeDirectoryStamps = func() {
		testHookBeforeDirectoryStamps = nil
		if err := os.Mkdir(filepath.Join(w.root, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Error(err)
		}
	}
}

// A directory created during capture is in its parent's listing without a
// stamp of its own. Files later created in it change only its stamp, so a
// hinted edit elsewhere must not reuse the selection.
func TestWatchPrepareRejectsDirectoryCreatedDuringCapture(t *testing.T) {
	for _, fixture := range relistFixtures {
		for _, dir := range []string{"fresh", "sub/fresh"} {
			t.Run(fixture.name+"/"+dir, func(t *testing.T) {
				w, b := fixture.prepare(t)
				createDuringCapture(t, w, dir)
				// Explicit selection lists directories, so its reselection
				// rejects the preparation. Git does not list empty directories.
				if _, _, _, err := w.Prepare(b); err != nil && !IsSourceChanged(err) {
					t.Fatal(err)
				}
				if testHookBeforeDirectoryStamps != nil {
					t.Fatal("preparation did not capture evidence")
				}
				writeFile(t, w.root, dir+"/new", "new")
				writeFile(t, w.root, "value", "edited")
				w.invalidatePath(filepath.Join(w.root, "value"), dirtyContent)
				assertPreparedMatchesFull(t, w, b)
			})
		}
	}
}

// Directories inside an excluded directory need no stamps, so one created
// there during capture keeps the evidence.
func TestGitWatchEvidenceKeepsDirectoryCreatedDuringCaptureInExcludedDirectory(t *testing.T) {
	w, b, _ := prepareGitWatchFixture(t)
	createDuringCapture(t, w, "build/fresh")
	assertPreparedMatchesFull(t, w, b)
	if testHookBeforeDirectoryStamps != nil {
		t.Fatal("preparation did not capture evidence")
	}
	writeFile(t, w.root, "value", "edited")
	assertIncremental(t, w, b, "value")
}
