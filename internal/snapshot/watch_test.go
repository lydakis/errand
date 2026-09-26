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
			assertWatchesOnlyCheckoutAndFiles(t, w, globals...)
			watched := map[string]bool{}
			for _, p := range w.w.WatchList() {
				watched[p] = true
			}
			for _, name := range globals {
				if file, err := filepath.EvalSymlinks(name); present && (err != nil || !watched[file]) {
					t.Errorf("existing global control %s is not watched", name)
				}
			}
		})
	}
}

// assertWatchesOnlyCheckoutAndFiles fails if the watch registers anything
// outside its checkout other than the files the given paths resolve to.
func assertWatchesOnlyCheckoutAndFiles(t *testing.T, w *Watch, files ...string) {
	t.Helper()
	allowed := map[string]bool{}
	for _, name := range files {
		if file, err := filepath.EvalSymlinks(name); err == nil {
			allowed[file] = true
		}
	}
	for _, p := range w.w.WatchList() {
		if rel, err := filepath.Rel(w.root, p); err == nil && filepath.IsLocal(rel) {
			continue
		}
		if info, err := os.Lstat(p); allowed[p] && err == nil && info.Mode().IsRegular() {
			continue
		}
		t.Errorf("watch registered %s outside the checkout", p)
	}
}

// watchGlobalControls starts a Git-selected watch of an idle checkout holding
// "keep" and "secret" after setup writes the user's global Git files. Global
// controls are polled quickly so each step can wait for the watch alone.
func watchGlobalControls(t *testing.T, setup func(home, xdg string)) *Watch {
	t.Helper()
	poll := externalControlPoll
	externalControlPoll = 10 * time.Millisecond
	t.Cleanup(func() { externalControlPoll = poll })
	home, xdg := isolateGlobalGit(t)
	setup(home, xdg)
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
	t.Cleanup(func() { w.Close() })
	return w
}

// settleWatch waits until the watch has stopped reporting changes.
func settleWatch(t *testing.T, w *Watch) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; {
		before := w.Generation()
		select {
		case err := <-w.Errors:
			t.Fatal(err)
		case <-time.After(100 * time.Millisecond):
		}
		select {
		case <-w.Changed:
		default:
		}
		if w.Generation() == before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("watch kept reporting changes to an idle checkout")
		}
	}
}

// expectQuiet requires change to leave a settled watch unchanged.
func expectQuiet(t *testing.T, w *Watch, change func()) {
	t.Helper()
	settleWatch(t, w)
	before := w.Generation()
	change()
	select {
	case <-w.Changed:
		t.Fatal("watch reported a change that cannot affect selection")
	case err := <-w.Errors:
		t.Fatal(err)
	case <-time.After(150 * time.Millisecond):
	}
	if w.Generation() != before {
		t.Fatal("watch reported a change that cannot affect selection")
	}
}

// expectGlobalChange requires change to a settled watch's global Git files to
// signal Changed by itself, and the next preparation to match fresh selection,
// which includes "secret" when included is true. The global controls that
// exist afterwards must be watched as files and nothing else outside the
// checkout.
func expectGlobalChange(t *testing.T, w *Watch, included bool, controls []string, change func()) {
	t.Helper()
	settleWatch(t, w)
	before := w.Generation()
	change()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for w.Generation() <= before {
		select {
		case <-w.Changed:
		case err := <-w.Errors:
			t.Fatal(err)
		case <-deadline.C:
			t.Fatal("idle checkout missed a global control change")
		}
	}
	m, _, _, err := w.Prepare(new(Builder))
	if err != nil {
		t.Fatal(err)
	}
	var prepared []string
	for _, e := range m.Entries {
		if e.Type != proto.EntryDir {
			prepared = append(prepared, e.Path)
		}
	}
	fresh, _, _, err := SelectFiles(w.root)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(prepared)
	slices.Sort(fresh)
	if !slices.Equal(prepared, fresh) {
		t.Fatalf("prepared %v, fresh selection %v", prepared, fresh)
	}
	if slices.Contains(fresh, "secret") != included {
		t.Fatalf("fresh selection %v, want secret included=%t", fresh, included)
	}
	for _, name := range controls {
		file, err := filepath.EvalSymlinks(name)
		if err != nil {
			continue
		}
		for deadline := time.Now().Add(3 * time.Second); !slices.Contains(w.w.WatchList(), file); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("existing global control %s is not watched", name)
			}
		}
	}
	assertWatchesOnlyCheckoutAndFiles(t, w, controls...)
}

// globalStep changes an idle checkout's global Git files. A quiet step must not
// be reported; any other must be, and must leave "secret" selected or not.
type globalStep struct {
	name     string
	quiet    bool
	included bool
	change   func()
}

func runGlobalSteps(t *testing.T, w *Watch, controls []string, steps []globalStep) {
	t.Helper()
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			if step.quiet {
				expectQuiet(t, w, step.change)
			} else {
				expectGlobalChange(t, w, step.included, controls, step.change)
			}
		})
	}
}

func TestWatchGitSelectionFollowsGlobalControlsWithoutTheirDirectories(t *testing.T) {
	t.Run("default excludes", func(t *testing.T) {
		var home, gitDir string
		w := watchGlobalControls(t, func(h, xdg string) {
			home, gitDir = h, filepath.Join(xdg, "git")
			writeFile(t, home, ".gitconfig", "")
		})
		controls := []string{filepath.Join(home, ".gitconfig"), filepath.Join(gitDir, "config"), filepath.Join(gitDir, "ignore")}
		ignore := controls[2]
		runGlobalSteps(t, w, controls, []globalStep{
			{name: "select while polling", quiet: true, change: func() {
				for range 3 {
					if _, _, _, err := SelectFiles(w.root); err != nil {
						t.Fatal(err)
					}
				}
			}},
			{name: "edit gitconfig", included: true, change: func() { writeFile(t, home, ".gitconfig", "[user]\n\tname = errand\n") }},
			{name: "create missing ignore", change: func() { writeFile(t, gitDir, "ignore", "secret\n") }},
			{name: "edit ignore in place", included: true, change: func() { writeFile(t, gitDir, "ignore", "other\n") }},
			{name: "replace ignore by rename", change: func() {
				writeFile(t, gitDir, ".ignore.tmp", "secret\n")
				rename(t, filepath.Join(gitDir, ".ignore.tmp"), ignore)
			}},
			{name: "delete ignore", included: true, change: func() { remove(t, ignore) }},
			{name: "recreate ignore", change: func() { writeFile(t, gitDir, "ignore", "secret\n") }},
			{name: "rename git directory away", included: true, change: func() { rename(t, gitDir, gitDir+".away") }},
			{name: "rename git directory back", change: func() { rename(t, gitDir+".away", gitDir) }},
			{name: "replace git directory", included: true, change: func() {
				writeFile(t, gitDir+".new", "ignore", "other\n")
				rename(t, gitDir, gitDir+".old")
				rename(t, gitDir+".new", gitDir)
			}},
			{name: "edit replaced file", quiet: true, change: func() { writeFile(t, gitDir+".old", "ignore", "unrelated\n") }},
			{name: "edit replacement in place", change: func() { writeFile(t, gitDir, "ignore", "secret\n") }},
		})
	})

	t.Run("symlinked excludes", func(t *testing.T) {
		// A dotfile manager links the configured excludes through a profile
		// link it swaps for each generation, leaving old generations intact.
		var home, excludes string
		w := watchGlobalControls(t, func(h, xdg string) {
			home, excludes = h, filepath.Join(h, "excludes")
			writeFile(t, home, "gen1/ignore", "secret\n")
			writeFile(t, home, "gen2/ignore", "other\n")
			writeFile(t, home, "plain/ignore", "unrelated\n")
			symlink(t, filepath.Join(home, "gen1"), filepath.Join(home, "profile"))
			symlink(t, filepath.Join(home, "profile", "ignore"), excludes)
			writeFile(t, home, ".gitconfig", "[core]\n\texcludesFile = "+excludes+"\n")
		})
		// swap atomically replaces link with a symlink to target.
		swap := func(target, link string) {
			symlink(t, target, link+".tmp")
			rename(t, link+".tmp", link)
		}
		runGlobalSteps(t, w, []string{filepath.Join(home, ".gitconfig"), excludes}, []globalStep{
			{name: "retarget intermediate link", included: true, change: func() { swap(filepath.Join(home, "gen2"), filepath.Join(home, "profile")) }},
			{name: "edit old target", quiet: true, change: func() { writeFile(t, home, "gen1/ignore", "unrelated\n") }},
			{name: "edit new target in place", change: func() { writeFile(t, home, "gen2/ignore", "secret\n") }},
			{name: "retarget excludes link", included: true, change: func() { swap(filepath.Join(home, "plain", "ignore"), excludes) }},
			{name: "replace excludes link", change: func() {
				remove(t, excludes)
				symlink(t, filepath.Join(home, "profile", "ignore"), excludes)
			}},
		})
	})
}

func rename(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

func remove(t *testing.T, name string) {
	t.Helper()
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
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
