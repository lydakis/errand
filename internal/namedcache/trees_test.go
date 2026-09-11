package namedcache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestTreeRestoreLayoutAndKeepConcurrentReaderFromPublishing(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1<<20)
	ctx := context.Background()
	key := Key{"owner", "project", "dependencies"}
	restore := func() (string, string, string) {
		t.Helper()
		id, workspace := proto.NewULID(), t.TempDir()
		if _, err := s.AcquireTree(ctx, key, id); err != nil {
			t.Fatal(err)
		}
		base, err := s.RestoreTree(ctx, key, id, workspace, "node_modules")
		if err != nil {
			t.Fatal(err)
		}
		return id, workspace, base
	}
	write := func(root, name, text string) {
		t.Helper()
		p := filepath.Join(root, "node_modules", name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p+".new", []byte(text), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(p+".new", p); err != nil {
			t.Fatal(err)
		}
	}
	id, a, base := restore()
	write(a, ".pnpm/tiny/node_modules/tiny/index.js", "module.exports = 42")
	write(a, ".modules.yaml", "virtualStoreDir: .pnpm\n")
	write(a, ".bin/tiny", "#!/bin/sh\n# "+a+"/node_modules/.pnpm/tiny\n")
	original, err := os.Stat(filepath.Join(a, "node_modules/.bin/tiny"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".pnpm/tiny/node_modules/tiny", filepath.Join(a, "node_modules/tiny")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(a, "package"), filepath.Join(a, "node_modules/local")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishTree(ctx, key, id, a, "node_modules", base, false); err != nil {
		t.Fatal(err)
	}
	reader, b, readerBase := restore()
	if actual, err := TreeFingerprint(ctx, b, "node_modules", readerBase); err != nil || actual != readerBase {
		t.Fatalf("restored comparison baseline differs: %q %q %v", actual, readerBase, err)
	}
	writer, c, writerBase := restore()
	info, err := os.Lstat(filepath.Join(b, "node_modules"))
	if err != nil || !info.IsDir() {
		t.Fatalf("real directory: %v %v", info, err)
	}
	if body, err := os.ReadFile(filepath.Join(b, "node_modules/tiny/index.js")); err != nil || string(body) != "module.exports = 42" {
		t.Fatalf("dependency: %s %v", body, err)
	}
	if target, _ := os.Readlink(filepath.Join(b, "node_modules/local")); target != filepath.Join(a, "package") {
		t.Fatalf("local target %q", target)
	}
	if body, _ := os.ReadFile(filepath.Join(b, "node_modules/.bin/tiny")); !strings.Contains(string(body), a+"/node_modules") {
		t.Fatalf("wrapper must remain unchanged: %s", body)
	}
	restored, err := os.Stat(filepath.Join(b, "node_modules/.bin/tiny"))
	if err != nil || restored.Mode() != original.Mode() || !restored.ModTime().Equal(original.ModTime()) {
		t.Fatalf("executable mode or modification time changed: %v %v", restored, err)
	}
	write(c, ".modules.yaml", "virtualStoreDir: .pnpm\nchanged: true\n")
	if _, err := s.PublishTree(ctx, key, writer, c, "node_modules", writerBase, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishTree(ctx, key, reader, b, "node_modules", readerBase, false); err != nil {
		t.Fatal(err)
	}
	_, next, _ := restore()
	if body, _ := os.ReadFile(filepath.Join(next, "node_modules/.modules.yaml")); !strings.Contains(string(body), "changed: true") {
		t.Fatalf("reader reverted a newer installation: %s", body)
	}
	if body, _ := os.ReadFile(filepath.Join(b, "node_modules/.modules.yaml")); strings.Contains(string(body), "changed: true") {
		t.Fatal("installation bookkeeping was shared")
	}
}

func TestTreeCancelledRestoreAndUnsafeDestination(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1<<20)
	ctx := context.Background()
	key, id := Key{"owner", "project", "deps"}, proto.NewULID()
	if _, err := s.AcquireTree(ctx, key, id); err != nil {
		t.Fatal(err)
	}
	workspace, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "package")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreTree(ctx, key, id, workspace, "package/node_modules"); err == nil {
		t.Fatal("followed external parent")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.RestoreTree(cancelled, key, id, workspace, "node_modules"); err == nil {
		t.Fatal("ignored cancellation")
	}
	if _, err := os.Lstat(filepath.Join(workspace, "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("cancelled restore exposed destination: %v", err)
	}
}

func TestTreeConsumesLegacyPayloadAndSurvivesEviction(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 0)
	ctx := t.Context()
	key, id := Key{"owner", "project", "arbitrary-directory"}, proto.NewULID()
	data, err := s.Acquire(ctx, key, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "current"), []byte("legacy"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, key, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireTree(ctx, key, id); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	base, err := s.RestoreTree(ctx, key, id, workspace, "outputs")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(workspace, "outputs/current"), filepath.Join(workspace, "outputs/saved")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishTree(ctx, key, id, workspace, "outputs", base, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(workspace, "outputs")); !os.IsNotExist(err) {
		t.Fatalf("consumed tree remained: %v", err)
	}
	if entries, err := os.ReadDir(data); err != nil || len(entries) != 0 {
		t.Fatalf("legacy payload was not retired: %v %v", entries, err)
	}
	if _, err := s.RestoreTree(ctx, key, id, workspace, "outputs"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseTree(ctx, key, id); err != nil {
		t.Fatal(err)
	}
	if gc, err := s.GC(ctx, false); err != nil || gc.Removed != 1 || gc.FreedBytes != 6 {
		t.Fatalf("snapshot accounting and eviction: %+v %v", gc, err)
	}
	if raw, err := os.ReadFile(filepath.Join(workspace, "outputs/saved")); err != nil || string(raw) != "legacy" {
		t.Fatalf("eviction damaged restored files: %q %v", raw, err)
	}
}
