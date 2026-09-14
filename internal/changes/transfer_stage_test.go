package changes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestTransferStageConcurrentFailureRestoresSource(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt=%v", corrupt), func(t *testing.T) {
			source, root := t.TempDir(), t.TempDir()
			paths := []string{"a", "a/b", "a/b/deep", "a/c", "a/c/deep"}
			for i := range 32 {
				name := fmt.Sprintf("a/%c/deep/file-%02d", 'b'+rune(i%2), i)
				paths = append(paths, name)
				if err := os.MkdirAll(filepath.Dir(filepath.Join(source, name)), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(source, name), []byte("content"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			sort.Strings(paths)
			m, err := snapshot.Build(source, paths)
			if err != nil {
				t.Fatal(err)
			}
			if corrupt {
				if err := os.WriteFile(filepath.Join(source, "a/b/deep/file-00"), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for i := len(m.Entries) - 1; i >= 0; i-- {
				m.Entries[i].Mode = 0
				if err := os.Chmod(filepath.Join(source, m.Entries[i].Path), 0); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { _ = RemoveTree(source) })
			target := transferTarget(t, root)
			s := TransferSession{Directory: t.TempDir(), Root: root, RootID: target.RootID, Owner: "owner", SourceID: "sender", MaxSourceBytes: 1 << 20, MaxChangeBytes: 1 << 20}
			t.Cleanup(func() { _ = RemoveTree(s.Directory) })
			if err := s.Initialize(t.Context(), t.TempDir(), proto.Manifest{}); err != nil {
				t.Fatal(err)
			}
			id := proto.NewULID()
			dir, b, err := s.Stage(t.Context(), id, source, m)
			if corrupt {
				if err == nil {
					t.Fatal("corrupt concurrent stage accepted")
				}
				if _, err := os.Stat(filepath.Join(s.Directory, "attempts", id)); !os.IsNotExist(err) {
					t.Fatalf("failed attempt published: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if err := VerifyExtracted(dir, b); err != nil {
				t.Fatal(err)
			}
			// Inspect ancestors before widening them to reach the next level.
			for _, e := range m.Entries {
				name := filepath.Join(source, e.Path)
				info, err := os.Stat(name)
				if err != nil || info.Mode().Perm() != 0 {
					t.Fatalf("source mode not restored at %s: %v, %v", e.Path, info, err)
				}
				if e.Type == proto.EntryDir {
					if err := os.Chmod(name, 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			v, err := s.Checkpoint().Read()
			if err != nil || v.Revision != 0 {
				t.Fatalf("staging changed checkpoint: %+v, %v", v, err)
			}
		})
	}
}

func TestTransferStagePublicationFailureRetry(t *testing.T) {
	root, b, staged := applyFixture(t, "before\n", "after!\n")
	target := transferTarget(t, root)
	s := TransferSession{Directory: t.TempDir(), Root: root, RootID: target.RootID, Owner: "owner", SourceID: "sender", MaxSourceBytes: 1 << 20, MaxChangeBytes: 1 << 20}
	if err := s.Initialize(t.Context(), filepath.Join(staged, "base"), b.BaseManifest); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("publication barrier failed")
	s.syncAttemptParent = func(string) error { return injected }
	id := proto.NewULID()
	if _, _, err := s.Stage(t.Context(), id, filepath.Join(staged, "remote"), b.RemoteManifest); !errors.Is(err, injected) {
		t.Fatalf("publication failure: %v", err)
	}
	if _, err := s.Attempt(id); !os.IsNotExist(err) {
		t.Fatalf("failed publication remains reusable: %v", err)
	}
	s.syncAttemptParent = nil
	dir, _, err := s.Stage(t.Context(), id, filepath.Join(staged, "remote"), b.RemoteManifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTransferBundle(dir); err != nil {
		t.Fatal(err)
	}
	// Even an existing attempt must not bypass a failing publication barrier.
	s.syncAttemptParent = func(string) error { return injected }
	if _, _, err := s.Stage(t.Context(), id, filepath.Join(staged, "remote"), b.RemoteManifest); !errors.Is(err, injected) {
		t.Fatalf("retry skipped publication barrier: %v", err)
	}
	v, err := s.Checkpoint().Read()
	if err != nil || v.Revision != 0 {
		t.Fatalf("failed publication changed checkpoint: %+v, %v", v, err)
	}
}

func TestTransferStagePreservesRestrictedTrees(t *testing.T) {
	base, source, root := t.TempDir(), t.TempDir(), t.TempDir()
	build := func(dir, body string) proto.Manifest {
		t.Helper()
		if err := os.Mkdir(filepath.Join(dir, "!dir"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "!dir", "file"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		link := "file"
		if dir == source {
			link = "other"
		}
		if err := os.Symlink(link, filepath.Join(dir, "!dir", "link")); err != nil {
			t.Fatal(err)
		}
		m, err := snapshot.Build(dir, []string{"!dir", "!dir/file", "!dir/link"})
		if err != nil {
			t.Fatal(err)
		}
		for i := range m.Entries {
			if m.Entries[i].Type != proto.EntrySymlink {
				m.Entries[i].Mode = 0
			}
		}
		if err := os.Chmod(filepath.Join(dir, "!dir", "file"), 0); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, "!dir"), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = RemoveTree(dir) })
		return m
	}
	before, after := build(base, "before\n"), build(source, "after\n")
	target := transferTarget(t, root)
	s := TransferSession{Directory: t.TempDir(), Root: root, RootID: target.RootID, Owner: "owner", SourceID: "sender", MaxSourceBytes: 1 << 20, MaxChangeBytes: 1 << 20}
	t.Cleanup(func() { _ = RemoveTree(s.Directory) })
	if err := s.Initialize(t.Context(), base, before); err != nil {
		t.Fatal(err)
	}
	dir, b, err := s.Stage(t.Context(), proto.NewULID(), source, after)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyExtracted(dir, b); err != nil {
		t.Fatal(err)
	}
	for _, tree := range []string{source, filepath.Join(dir, "base"), filepath.Join(dir, "remote")} {
		info, err := os.Stat(filepath.Join(tree, "!dir"))
		if err != nil || info.Mode().Perm() != 0 {
			t.Fatalf("directory permissions: %v, %v", info, err)
		}
	}
	access, err := makeTreeAccessible(source)
	if err != nil {
		t.Fatal(err)
	}
	writeErr := access.root.Chmod("!dir/file", 0600)
	if writeErr == nil {
		writeErr = os.WriteFile(filepath.Join(source, "!dir", "file"), []byte("later\n"), 0600)
	}
	restoreErr := access.restore()
	if writeErr != nil || restoreErr != nil {
		t.Fatalf("changing source after staging: %v, %v", writeErr, restoreErr)
	}
	if err := VerifyExtracted(dir, b); err != nil {
		t.Fatalf("source mutation changed the independent stage: %v", err)
	}
}

func TestTransferStageFailureDoesNotPublish(t *testing.T) {
	for _, failure := range []string{"source-content", "source-type", "base-content", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			root, b, staged := applyFixture(t, "before\n", "after!\n")
			target := transferTarget(t, root)
			s := TransferSession{Directory: t.TempDir(), Root: root, RootID: target.RootID, Owner: "owner", SourceID: "sender", MaxSourceBytes: 1 << 20, MaxChangeBytes: 1 << 20}
			if err := s.Initialize(t.Context(), filepath.Join(staged, "base"), b.BaseManifest); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch failure {
			case "source-content":
				if err := os.WriteFile(filepath.Join(staged, "remote", "artifact"), []byte("broken\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "base-content":
				if err := os.WriteFile(filepath.Join(s.Blobs().Directory, b.BaseManifest.Entries[0].SHA256), []byte("broken\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "source-type":
				name := filepath.Join(staged, "remote", "artifact")
				if err := os.Rename(name, name+"-body"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("artifact-body", name); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			}
			id := proto.NewULID()
			if _, _, err := s.Stage(ctx, id, filepath.Join(staged, "remote"), b.RemoteManifest); err == nil {
				t.Fatal("invalid stage accepted")
			}
			if _, err := os.Stat(filepath.Join(s.Directory, "attempts", id)); !os.IsNotExist(err) {
				t.Fatalf("failed attempt published: %v", err)
			}
			v, err := s.Checkpoint().Read()
			if err != nil || v.Revision != 0 {
				t.Fatalf("failed staging changed checkpoint: %+v, %v", v, err)
			}
		})
	}
}
