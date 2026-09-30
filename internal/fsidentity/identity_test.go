package fsidentity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIdentityDistinguishesFilesAndFollowsHardLinks(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte(path), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	firstID, _, err := Lstat(first)
	if err != nil {
		t.Fatal(err)
	}
	secondID, _, err := Lstat(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstID.IsZero() || secondID.IsZero() {
		t.Fatalf("zero identity: %+v %+v", firstID, secondID)
	}
	if firstID == secondID {
		t.Fatalf("distinct files share identity %+v", firstID)
	}

	f, err := os.Open(first)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	openedID, err := FromInfo(opened)
	if err != nil {
		t.Fatal(err)
	}
	if openedID != firstID {
		t.Fatalf("opened identity %+v, path identity %+v", openedID, firstID)
	}

	link := filepath.Join(dir, "link")
	if err := os.Link(first, link); err != nil {
		t.Fatal(err)
	}
	linkID, _, err := Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if linkID != firstID {
		t.Fatalf("hard link identity %+v, original %+v", linkID, firstID)
	}
}

func TestIdentityThroughRootMatchesPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	info, err := root.Lstat("child")
	if err != nil {
		t.Fatal(err)
	}
	rootID, err := FromInfo(info)
	if err != nil {
		t.Fatal(err)
	}
	pathID, _, err := Lstat(filepath.Join(dir, "child"))
	if err != nil {
		t.Fatal(err)
	}
	if rootID != pathID {
		t.Fatalf("root identity %+v, path identity %+v", rootID, pathID)
	}
}
