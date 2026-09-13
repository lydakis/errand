package snapshot

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
