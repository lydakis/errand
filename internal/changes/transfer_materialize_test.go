package changes

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestTransferMaterializationRejectsAliasedSymlinkParent(t *testing.T) {
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "probe"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(dest, "PROBE"))
	if os.IsNotExist(err) {
		t.Skip("requires a case-insensitive filesystem")
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dest, "probe")); err != nil {
		t.Fatal(err)
	}
	tree, err := os.OpenRoot(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	m := proto.Manifest{Entries: []proto.ManifestEntry{
		// The target must exist before the alias. Otherwise the old writer can
		// fail on a dangling link instead of exposing the traversal regression.
		{Path: "0-target", Type: proto.EntryDir, Mode: 0700},
		{Path: "Link", Type: proto.EntrySymlink, Target: "0-target"},
		{Path: "link/file", Type: proto.EntryFile, Mode: 0600, Size: 4, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("body")))},
	}}
	err = materializeTransferTree(t.Context(), tree, m, durableMaterialization(func() error { t.Fatal("aliased tree reached publication barrier"); return nil }), func(proto.ManifestEntry) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("body")), nil
	})
	if err == nil {
		t.Fatal("symlink alias accepted")
	}
	if _, err := os.Stat(filepath.Join(dest, "0-target", "file")); !os.IsNotExist(err) {
		t.Fatalf("write traversed symlink: %v", err)
	}
}

func TestTransferMaterializationDoesNotFlushRejectedContent(t *testing.T) {
	tree, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	m := proto.Manifest{Entries: []proto.ManifestEntry{{Path: "file", Type: proto.EntryFile, Mode: 0600, Size: 4, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("good")))}}}
	err = materializeTransferTree(t.Context(), tree, m, materializationPolicy{permissions: manifestPermissions, syncData: func(*os.File) error { t.Fatal("flushed rejected content"); return nil }, barrier: func() error { t.Fatal("published rejected content"); return nil }}, func(proto.ManifestEntry) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("evil")), nil
	})
	if err == nil {
		t.Fatal("corrupt content accepted")
	}
}

func TestTransferMaterializationDurability(t *testing.T) {
	for _, failure := range []string{"none", "member", "barrier", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			source, dest := t.TempDir(), t.TempDir()
			if err := os.Mkdir(filepath.Join(source, "dir"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "dir", "file"), []byte("content"), 0600); err != nil {
				t.Fatal(err)
			}
			m, err := snapshot.Build(source, []string{"dir", "dir/file"})
			if err != nil {
				t.Fatal(err)
			}
			for i := range m.Entries {
				m.Entries[i].Mode = 0
			}
			tree, err := os.OpenRoot(dest)
			if err != nil {
				t.Fatal(err)
			}
			defer tree.Close()
			t.Cleanup(func() { _ = RemoveTree(dest) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			injected := errors.New("injected materialization failure")
			var members atomic.Int32
			barriers := 0
			policy := materializationPolicy{permissions: manifestPermissions, syncData: func(f *os.File) error {
				members.Add(1)
				info, err := f.Stat()
				if err != nil {
					return err
				}
				if info.Mode().Perm() != 0 {
					t.Errorf("synced before final mode: %v", info.Mode())
				}
				if failure == "member" {
					return injected
				}
				if failure == "cancel" {
					cancel()
				}
				return syncStagedData(f)
			}, barrier: func() error {
				barriers++
				if members.Load() != 2 {
					t.Errorf("barrier before all members: %d", members.Load())
				}
				if failure == "barrier" {
					return injected
				}
				return syncApplyRootDirectory(tree, ".")
			}}
			err = materializeTransferTree(ctx, tree, m, policy, func(e proto.ManifestEntry) (io.ReadCloser, error) { return os.Open(filepath.Join(source, e.Path)) })
			switch failure {
			case "none":
				if err != nil || barriers != 1 {
					t.Fatalf("success: %v, %d barriers", err, barriers)
				}
			case "barrier":
				if !errors.Is(err, injected) || barriers != 1 {
					t.Fatalf("barrier failure: %v, %d barriers", err, barriers)
				}
			case "member":
				if !errors.Is(err, injected) || barriers != 0 {
					t.Fatalf("member failure: %v, %d barriers", err, barriers)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) || barriers != 0 {
					t.Fatalf("cancellation: %v, %d barriers", err, barriers)
				}
			}
		})
	}
}

// A case-folding alias can already have its explicit mode when an implicit
// parent is finalized. That implicit spelling must not reset the shared inode.
func TestImplicitDirectoryFinalizationPreservesExistingMode(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dir"), 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "dir"), 0700) })
	tree, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	paths := materializationPaths{root: tree}
	defer paths.close()
	synced := false
	err = finalizeMaterializedDirectories(t.Context(), &paths,
		map[string]materializedDirectory{"dir": {}}, func(f *os.File) error {
			synced = true
			info, err := f.Stat()
			if err == nil && info.Mode().Perm() != 0500 {
				t.Errorf("implicit parent reset mode to %o", info.Mode().Perm())
			}
			return err
		})
	if err != nil || !synced {
		t.Fatalf("implicit directory durability: synced=%v, error=%v", synced, err)
	}
}
