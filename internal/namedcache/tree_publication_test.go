package namedcache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestSharedTreeReaderDoesNotRepublishSharedMetadata(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1<<20)
	key := Key{"owner", "project", "deps"}
	restore := func() (string, string, string) {
		t.Helper()
		id, dir := proto.NewULID(), t.TempDir()
		if _, err := s.AcquireTree(t.Context(), key, id); err != nil {
			t.Fatal(err)
		}
		base, err := s.restoreTree(t.Context(), key, id, dir, "cache", true)
		if err != nil {
			t.Fatal(err)
		}
		return id, dir, base
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	publish := func(id, dir, base string) string {
		t.Helper()
		next, err := s.PublishTree(t.Context(), key, id, dir, "cache", base, false)
		must(err)
		return next
	}
	seed, dir, base := restore()
	must(os.WriteFile(filepath.Join(dir, "cache/x"), []byte("old-x"), 0600))
	must(os.WriteFile(filepath.Join(dir, "cache/y"), []byte("old-y"), 0600))
	publish(seed, dir, base)
	a, ad, ab := restore()
	b, bd, bb := restore()
	if !strings.HasPrefix(ab, "links:") || !strings.HasPrefix(bb, "links:") {
		t.Fatalf("readers must restore linked trees: %q %q", ab, bb)
	}
	ai, err := os.Stat(filepath.Join(ad, "cache/x"))
	must(err)
	bi, err := os.Stat(filepath.Join(bd, "cache/x"))
	must(err)
	if !os.SameFile(ai, bi) {
		t.Fatal("fixture files must share an inode")
	}
	must(os.WriteFile(filepath.Join(ad, "cache/x"), []byte("longer-new-x"), 0600))
	must(os.Chmod(filepath.Join(ad, "cache/x"), 0755))
	must(os.WriteFile(filepath.Join(ad, "cache/y.new"), []byte("new-y"), 0600))
	must(os.Rename(filepath.Join(ad, "cache/y.new"), filepath.Join(ad, "cache/y")))
	publish(a, ad, ab)
	if next := publish(b, bd, bb); next != bb {
		t.Fatal("reader published shared metadata changes")
	}
	_, cd, _ := restore()
	y, err := os.ReadFile(filepath.Join(cd, "cache/y"))
	must(err)
	if string(y) != "new-y" {
		t.Fatalf("reader reverted new layout: %q", y)
	}
}

func TestEmptyTreeUsesPrivateComparison(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1<<20)
	key, id := Key{"owner", "project", "empty"}, proto.NewULID()
	if _, err := s.AcquireTree(t.Context(), key, id); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	base, err := s.restoreTree(t.Context(), key, id, workspace, "cache", true)
	if err != nil || !strings.HasPrefix(base, "private:") {
		t.Fatalf("empty tree has no linked files: %q %v", base, err)
	}
	actual, err := TreeFingerprint(t.Context(), workspace, "cache", base)
	if err != nil || actual != base {
		t.Fatalf("empty comparison baseline differs: %q %q %v", base, actual, err)
	}
}

func TestPrivateTreeDetectsInPlaceWrites(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1<<20)
	key, id := Key{"owner", "project", "private"}, proto.NewULID()
	if _, err := s.AcquireTree(t.Context(), key, id); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	base, err := s.restoreTree(t.Context(), key, id, workspace, "cache", false)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(workspace, "cache/value")
	if err := os.WriteFile(file, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	base, err = s.PublishTree(t.Context(), key, id, workspace, "cache", base, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("updated in place"), 0600); err != nil {
		t.Fatal(err)
	}
	next, err := s.PublishTree(t.Context(), key, id, workspace, "cache", base, false)
	if err != nil || next == base {
		t.Fatalf("private mutation not saved: %q %v", next, err)
	}
	other := t.TempDir()
	base, err = s.restoreTree(t.Context(), key, id, other, "cache", false)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := TreeFingerprint(t.Context(), other, "cache", base)
	if err != nil || actual != base {
		t.Fatalf("restored metadata differs: %q %q %v", base, actual, err)
	}
	value, err := os.ReadFile(filepath.Join(other, "cache/value"))
	if err != nil || string(value) != "updated in place" {
		t.Fatalf("private contents: %q %v", value, err)
	}
}
