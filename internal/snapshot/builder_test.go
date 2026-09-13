package snapshot

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuilderInvalidatesHashesAfterBackdatedAndAtomicEdits(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "value")
	if err := os.WriteFile(name, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(name)
	var builder Builder
	check := func() string {
		t.Helper()
		got, err := builder.Build(root, []string{"value"})
		if err != nil {
			t.Fatal(err)
		}
		want, err := Build(root, []string{"value"})
		if err != nil {
			t.Fatal(err)
		}
		if got.RootHash() != want.RootHash() {
			t.Fatal("cached hash hid a source change")
		}
		return got.RootHash()
	}
	initial := check()
	if check() != initial {
		t.Fatal("unchanged scan differs")
	}
	time.Sleep(time.Millisecond)
	if err := os.WriteFile(name, []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if check() == initial {
		t.Fatal("same-size, backdated edit was lost")
	}
	if err := os.WriteFile(name+".new", []byte("atomic"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(name+".new", name); err != nil {
		t.Fatal(err)
	}
	check()
	if err := os.Chmod(name, 0700); err != nil {
		t.Fatal(err)
	}
	check()
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	m, err := builder.Build(root, nil)
	if err != nil || len(m.Entries) != 0 {
		t.Fatalf("deletion: %+v %v", m, err)
	}
}
