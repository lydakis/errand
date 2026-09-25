package snapshot

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

func TestWatchRegistersFilesSelectedDuringStartup(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	git("init", "-q")
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored/\n"), 0600)
	os.Mkdir(filepath.Join(root, "ignored"), 0700)
	name := filepath.Join(root, "ignored", "value")
	os.WriteFile(name, []byte("before"), 0600)
	w, err := watchFiles(root, SelectOptions{}, func(root string, opts SelectOptions) ([]string, GitInfo, proto.SelectionPolicy, error) {
		paths, gi, policy, err := SelectFilesWithOptions(root, opts)
		// Index changes after initial selection, before any native watch exists.
		git("add", "-f", "ignored/value")
		return paths, gi, policy, err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	found := false
	for _, p := range w.w.WatchList() {
		if p == filepath.Join(w.root, "ignored") {
			found = true
		}
	}
	if !found {
		t.Fatal("initial snapshot can include a tracked directory without watching it")
	}
	before := w.Generation()
	if err := os.WriteFile(name, []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for w.Generation() == before {
		select {
		case <-w.Changed:
		case err := <-w.Errors:
			t.Fatal(err)
		case <-deadline:
			t.Fatal("newly selected file edit was missed")
		}
	}
}

func TestWatchFilesTracksAtomicSavesNewDirectoriesAndModes(t *testing.T) {
	root := t.TempDir()
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(".errandignore", "ignored/\n")
	write("value", "old")
	w, err := WatchFiles(root, SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	await := func(before uint64) {
		t.Helper()
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		for w.Generation() <= before {
			select {
			case <-w.Changed:
			case err := <-w.Errors:
				t.Fatal(err)
			case <-deadline.C:
				t.Fatal("missed source change")
			}
		}
	}
	for i := range 2 {
		before := w.Generation()
		write(".save", "new")
		if err := os.Rename(filepath.Join(root, ".save"), filepath.Join(root, "value")); err != nil {
			t.Fatal(err)
		}
		await(before)
		// Let the native backend finish the rename before exercising the new inode.
		time.Sleep(50 * time.Millisecond)
		before = w.Generation()
		write("value", string(rune('a'+i)))
		await(before)
	}
	before := w.Generation()
	if err := os.MkdirAll(filepath.Join(root, "new", "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	write("new/nested/value", "first")
	await(before)
	time.Sleep(100 * time.Millisecond)
	before = w.Generation()
	write("new/nested/value", "second")
	await(before)
	before = w.Generation()
	if err := os.Chmod(filepath.Join(root, "value"), 0700); err != nil {
		t.Fatal(err)
	}
	await(before)
}

func TestWatchGitSelectionIncludesNewlyTrackedIgnoredFilesWithoutFeedback(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	git("init", "-q")
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored/\n"), 0600)
	os.Mkdir(filepath.Join(root, "ignored"), 0700)
	os.WriteFile(filepath.Join(root, "ignored", "value"), []byte("old"), 0600)
	w, err := WatchFiles(root, SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	git("add", "-f", "ignored/value")
	// Wait for selection refresh to register the formerly ignored directory.
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case err := <-w.Errors:
			t.Fatal(err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("newly tracked directory was not watched")
		}
		found := false
		for _, p := range w.w.WatchList() {
			if strings.HasSuffix(p, "/ignored") {
				found = true
			}
		}
		if found {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Drain the index/add notifications before checking ordinary content edits.
	time.Sleep(100 * time.Millisecond)
	before := w.Generation()
	os.WriteFile(filepath.Join(root, "ignored", "value"), []byte("new"), 0600)
	for w.Generation() == before {
		if time.Now().After(deadline) {
			t.Fatal("tracked ignored file edit missed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	before = w.Generation()
	for range 3 {
		if _, _, _, err := SelectFiles(root); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if w.Generation() != before {
		t.Fatal("snapshot selection created its own watcher events")
	}
}

func TestWatchFilesIdleAndIgnoredChurnAreQuiet(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".errandignore"), []byte("ignored/\n"), 0600)
	os.Mkdir(filepath.Join(root, "ignored"), 0700)
	w, err := WatchFiles(root, SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := range 100 {
		os.WriteFile(filepath.Join(root, "ignored", "build"), []byte{byte(i)}, 0600)
	}
	select {
	case <-w.Changed:
		t.Fatal("ignored writes invalidated the source")
	case err := <-w.Errors:
		t.Fatal(err)
	case <-time.After(250 * time.Millisecond):
	}
}

// isolateGlobalGit points Git's user-level configuration at fresh directories.
func isolateGlobalGit(t *testing.T) (home, xdg string) {
	t.Helper()
	home, xdg = t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_GLOBAL", "")
	os.Unsetenv("GIT_CONFIG_GLOBAL")
	return home, xdg
}

func TestWatchGitSelectionNeverWatchesGlobalControlDirectories(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprintf("present=%t", present), func(t *testing.T) {
			home, xdg := isolateGlobalGit(t)
			// Entries a kqueue watch on $HOME would open: a FIFO blocks readers
			// and macOS guards folders like Desktop behind a privacy prompt.
			if err := syscall.Mkfifo(filepath.Join(home, "fifo"), 0600); err != nil {
				t.Fatal(err)
			}
			writeFile(t, home, "Desktop/private", "unrelated")
			globals := []string{filepath.Join(home, ".gitconfig"), filepath.Join(xdg, "git", "config"), filepath.Join(xdg, "git", "ignore")}
			if present {
				for _, name := range globals {
					writeFile(t, filepath.Dir(name), filepath.Base(name), "")
				}
			}
			root := t.TempDir()
			if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			w, err := WatchFiles(root, SelectOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			watched := map[string]bool{}
			for _, p := range w.w.WatchList() {
				watched[p] = true
				if rel, err := filepath.Rel(w.root, p); err == nil && filepath.IsLocal(rel) {
					continue
				}
				if present && slices.Contains(globals, p) {
					continue
				}
				t.Errorf("watch registered %s outside the checkout", p)
			}
			for _, name := range globals {
				if present && !watched[name] {
					t.Errorf("existing global control %s is not watched", name)
				}
			}
		})
	}
}

func TestWatchGitSelectionFollowsGlobalControlsWithoutTheirDirectories(t *testing.T) {
	home, xdg := isolateGlobalGit(t)
	writeFile(t, home, ".gitconfig", "")
	ignore := filepath.Join(xdg, "git", "ignore")
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	writeFile(t, root, "keep", "kept")
	writeFile(t, root, "secret", "ignored")
	w, err := WatchFiles(root, SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	b := new(Builder)
	selected := func() bool {
		t.Helper()
		m, _, _, _, err := w.Prepare(b)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(m.Entries, func(e proto.ManifestEntry) bool { return e.Path == "secret" })
	}
	await := func(before uint64) {
		t.Helper()
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		for w.Generation() <= before {
			select {
			case <-w.Changed:
			case err := <-w.Errors:
				t.Fatal(err)
			case <-deadline.C:
				t.Fatal("missed global control change")
			}
		}
	}
	awaitWatched := func(name string) {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Second); !slices.Contains(w.w.WatchList(), name); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s is not watched", name)
			}
		}
	}
	if !selected() {
		t.Fatal("selection lost an unignored file")
	}
	// Selection reads the watched global files without invalidating itself.
	time.Sleep(100 * time.Millisecond)
	before := w.Generation()
	for range 3 {
		if _, _, _, err := SelectFiles(root); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if w.Generation() != before {
		t.Fatal("reading global controls created watcher events")
	}
	before = w.Generation()
	writeFile(t, home, ".gitconfig", "[user]\n\tname = errand\n")
	await(before)

	// Nothing watches a missing global file, but preparation rereads it.
	writeFile(t, filepath.Dir(ignore), filepath.Base(ignore), "secret\n")
	if selected() {
		t.Fatal("selection kept a file a new global exclude ignores")
	}
	// The next refresh, here for a new directory, watches the created file.
	if err := os.Mkdir(filepath.Join(root, "new"), 0700); err != nil {
		t.Fatal(err)
	}
	awaitWatched(ignore)
	time.Sleep(50 * time.Millisecond)
	before = w.Generation()
	writeFile(t, filepath.Dir(ignore), ".ignore.tmp", "other\n")
	if err := os.Rename(filepath.Join(filepath.Dir(ignore), ".ignore.tmp"), ignore); err != nil {
		t.Fatal(err)
	}
	await(before)
	if !selected() {
		t.Fatal("selection kept excluding a file after the global exclude changed")
	}
	// Refresh re-adds the replaced file, so in-place edits keep invalidating.
	time.Sleep(100 * time.Millisecond)
	awaitWatched(ignore)
	before = w.Generation()
	writeFile(t, filepath.Dir(ignore), filepath.Base(ignore), "secret\n")
	await(before)
	if selected() {
		t.Fatal("selection kept a file the global exclude now ignores")
	}
}

func BenchmarkWatchFilesStart(b *testing.B) {
	root := b.TempDir()
	os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0600)
	for i := range 10000 {
		os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%05d", i)), []byte("body"), 0600)
	}
	b.ResetTimer()
	for b.Loop() {
		w, err := WatchFiles(root, SelectOptions{})
		if err != nil {
			b.Fatal(err)
		}
		w.Close()
	}
}
