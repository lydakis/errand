package changes

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestMaterializeMergeInputPreservesRestrictedSource(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		name := "valid"
		if corrupt {
			name = "corrupt"
		}
		t.Run(name, func(t *testing.T) {
			source, dest := t.TempDir(), t.TempDir()
			t.Cleanup(func() { _ = RemoveTree(source); _ = RemoveTree(dest) })
			if err := os.Mkdir(filepath.Join(source, "dir"), 0700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(source, "dir/file")
			if err := os.WriteFile(file, []byte("before"), 0600); err != nil {
				t.Fatal(err)
			}
			m, err := snapshot.Build(source, []string{"dir", "dir/file"})
			if err != nil {
				t.Fatal(err)
			}
			if corrupt {
				if err := os.WriteFile(file, []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for i := len(m.Entries) - 1; i >= 0; i-- {
				m.Entries[i].Mode = 0
				if err := os.Chmod(filepath.Join(source, m.Entries[i].Path), 0); err != nil {
					t.Fatal(err)
				}
			}
			err = materializeMergeInput(source, dest, m)
			if corrupt && err == nil || !corrupt && err != nil {
				t.Fatalf("materialization: %v", err)
			}
			for _, entry := range m.Entries {
				path := filepath.Join(source, entry.Path)
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0 {
					t.Fatalf("source mode not restored: %s, %v", entry.Path, err)
				}
				if entry.Type == proto.EntryDir {
					if err := os.Chmod(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			if corrupt {
				return
			}
			if err := os.Chmod(file, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte("edited"), 0600); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(filepath.Join(dest, "dir/file"))
			if err != nil || string(body) != "before" {
				t.Fatalf("source edit changed merge input: %q, %v", body, err)
			}
		})
	}
}

func TestApplyThreeWayMergesRestrictedInputs(t *testing.T) {
	local, bundle, staged := applyFixture(t, "first\nsecond\nthird\n", "FIRST\nsecond\nthird\n")
	t.Cleanup(func() { _ = RemoveTree(staged) })
	// Keep both staged inputs unreadable while local content and permissions
	// diverge. Neither side can be selected wholesale: merging must read both
	// private copies and preserve the local mode from the three-way mode merge.
	bundle.BaseManifest.Entries[0].Mode = 0
	bundle.RemoteManifest.Entries[0].Mode = 0
	bundle.BaselineRoot = bundle.BaseManifest.RootHash()
	for _, tree := range []string{"base", "remote"} {
		if err := os.Chmod(filepath.Join(staged, tree, "artifact"), 0); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(local, "artifact")
	if err := os.WriteFile(file, []byte("first\nsecond\nTHIRD\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0400); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction(), ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := CommitApply(local, result.Transaction); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(file)
	if err != nil || string(body) != "FIRST\nsecond\nTHIRD\n" {
		t.Fatalf("merged artifact = %q, %v", body, err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0400 {
		t.Fatalf("merged mode = %o, want 400", info.Mode().Perm())
	}
	for _, tree := range []string{"base", "remote"} {
		info, err := os.Stat(filepath.Join(staged, tree, "artifact"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0 {
			t.Fatalf("%s source mode = %o, want 0", tree, info.Mode().Perm())
		}
	}
}

// Physical scratch permissions must never replace the remote manifest's modes.
func TestApplyMergeInputsPreserveLogicalModes(t *testing.T) {
	remote, jobDir, local := t.TempDir(), t.TempDir(), t.TempDir()
	t.Cleanup(func() { _ = RemoveTree(remote); _ = RemoveTree(local) })
	baseline := proto.Manifest{}
	if err := CaptureWorkspaceBaseContext(t.Context(), remote, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(remote, "dir/nested"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{"read": 0400, "exec": 0755, "sealed": 0} {
		file := filepath.Join(remote, "dir/nested", name)
		if err := os.WriteFile(file, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(file, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("exec", filepath.Join(remote, "dir/nested/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(remote, "dir/nested"), 0500); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := CollectWorkspaceChangesContext(t.Context(), remote, jobDir, baseline, proto.SelectionPolicy{}, 1<<20)
	if err != nil || !collected {
		t.Fatalf("collect: %t, %v", collected, err)
	}
	staged := extractTestBundle(t, jobDir, bundle)
	t.Cleanup(func() { _ = RemoveTree(staged) })
	result, err := Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction(), ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := CommitApply(local, result.Transaction); err != nil {
		t.Fatal(err)
	}
	for _, entry := range bundle.RemoteManifest.Entries {
		file := filepath.Join(local, entry.Path)
		info, err := os.Lstat(file)
		if err != nil {
			t.Fatal(err)
		}
		if entry.Type == proto.EntrySymlink {
			target, err := os.Readlink(file)
			if err != nil || target != entry.Target {
				t.Fatalf("link: %q, %v", target, err)
			}
			continue
		}
		if uint32(info.Mode().Perm()) != entry.Mode {
			t.Fatalf("%s mode = %o, want %o", entry.Path, info.Mode().Perm(), entry.Mode)
		}
		if entry.Type == proto.EntryFile {
			if err := os.Chmod(file, 0600); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(file)
			if err != nil || string(body) != filepath.Base(file) {
				t.Fatalf("%s body: %q, %v", entry.Path, body, err)
			}
		}
	}
}
