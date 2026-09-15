package changes

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializationParentsBoundHandlesAndPreserveRoot(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 2*stagingWorkers; i++ {
		name := fmt.Sprintf("parent/dir-%d/file", i)
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	paths := materializationPaths{root: root, verify: true}
	defer paths.close()
	held, _, heldLease, err := paths.parent("parent/dir-0/file")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2*stagingWorkers; i++ {
		name := fmt.Sprintf("parent/dir-%d/file", i)
		parent, leaf, lease, err := paths.parent(name)
		if err != nil {
			t.Fatal(err)
		}
		body, err := parent.ReadFile(leaf)
		paths.release(lease)
		if err != nil || string(body) != name {
			t.Fatalf("read %q: %q, %v", name, body, err)
		}
		if len(paths.parents) > stagingWorkers {
			t.Fatal("unbounded retained directory handles")
		}
	}
	if _, err := held.ReadFile("file"); err != nil {
		t.Fatalf("evicted a borrowed parent: %v", err)
	}
	paths.release(heldLease)
	if err := paths.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := held.Lstat("."); err == nil {
		t.Fatal("retained parent handle leaked")
	}
	if _, err := root.Lstat("parent"); err != nil {
		t.Fatalf("closed caller's root: %v", err)
	}
}

func TestMaterializationParentsRejectReboundSource(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a/b/c"), 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	paths := materializationPaths{root: root, verify: true}
	_, _, lease, err := paths.parent("a/b/file")
	if err != nil {
		t.Fatal(err)
	}
	paths.release(lease)
	_, _, lease, err = paths.parent("a/b/c/file")
	if err != nil {
		t.Fatal(err)
	}
	paths.release(lease)
	if err := os.Rename(filepath.Join(dir, "a/b"), filepath.Join(dir, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "a/b"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := paths.close(); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("rebound parent accepted: %v", err)
	}
}

func TestMaterializationParentsBusyFallbackAndEvictionFailure(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i <= stagingWorkers; i++ {
		if err := os.MkdirAll(filepath.Join(dir, fmt.Sprintf("parent/%02d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	paths := materializationPaths{root: root, verify: true}
	var leases []*materializationParent
	defer func() {
		for _, lease := range leases {
			paths.release(lease)
		}
		paths.close()
	}()
	for i := 0; i < stagingWorkers; i++ {
		_, _, lease, err := paths.parent(fmt.Sprintf("parent/%02d/file", i))
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	name := fmt.Sprintf("parent/%02d/file", stagingWorkers)
	parent, leaf, lease, err := paths.parent(name)
	if err != nil || parent != root || leaf != name || lease != nil {
		t.Fatalf("busy fallback: %v, %q, %v, %v", parent, leaf, lease, err)
	}
	if err := os.Rename(filepath.Join(dir, "parent/00"), filepath.Join(dir, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "parent/00"), 0700); err != nil {
		t.Fatal(err)
	}
	paths.release(leases[0])
	leases[0] = nil
	_, _, lease, err = paths.parent(name)
	paths.release(lease)
	if !errors.Is(err, errMaterializationParentVerification) || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("accepted rebound eviction: %v", err)
	}
}
