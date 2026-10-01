package changes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/fsmode"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

// A Windows runner receives modes from a Unix client but can't store them.
// Results must keep them and report only what the job changed.
func TestCollectionKeepsSubmittedModesOnWindows(t *testing.T) {
	if !fsmode.Logical {
		t.Skip("file system stores POSIX modes")
	}
	workspace := t.TempDir()
	jobDir := t.TempDir()
	files := map[string]string{"run.sh": "echo hi\n", "lib.txt": "v1\n", "bin/tool": "tool\n", "locked.txt": "keep\n"}
	if err := os.Mkdir(filepath.Join(workspace, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(workspace, filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	locked := filepath.Join(workspace, "locked.txt")
	if err := os.Chmod(locked, 0o444); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o644)
	baseline, err := snapshot.Build(workspace, []string{"bin", "bin/tool", "lib.txt", "locked.txt", "run.sh"})
	if err != nil {
		t.Fatal(err)
	}
	submitted := map[string]uint32{"bin": 0o755, "bin/tool": 0o755, "lib.txt": 0o644, "locked.txt": 0o444, "run.sh": 0o755}
	for i := range baseline.Entries {
		baseline.Entries[i].Mode = submitted[baseline.Entries[i].Path]
	}
	if err := CaptureWorkspaceBaseContext(context.Background(), workspace, jobDir, baseline); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(workspace, "lib.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "bin", "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := CollectWorkspaceChangesContext(context.Background(), workspace, jobDir, baseline, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !collected || fmt.Sprint(bundle.Paths) != "[bin/new.txt lib.txt]" {
		t.Fatalf("collected = %t, paths = %v", collected, bundle.Paths)
	}
	want := map[string]uint32{"bin": 0o755, "bin/new.txt": 0o644, "lib.txt": 0o644}
	for _, entry := range bundle.RemoteManifest.Entries {
		if mode, ok := want[entry.Path]; ok && entry.Mode != mode {
			t.Fatalf("remote mode for %s = %#o, want %#o", entry.Path, entry.Mode, mode)
		}
	}
	staged := extractTestBundle(t, jobDir, bundle)
	if err := VerifyExtracted(staged, bundle); err != nil {
		t.Fatal(err)
	}
}

func TestApplyKeepsSubmittedModesOnWindows(t *testing.T) {
	if !fsmode.Logical {
		t.Skip("file system stores POSIX modes")
	}
	for _, tc := range []struct {
		name         string
		delete       bool
		localContent string
		localMode    os.FileMode
		conflict     bool
	}{
		{name: "delete-unchanged-executable", delete: true, localContent: "\x00base", localMode: 0o644},
		{name: "update-unchanged-binary", localContent: "\x00base", localMode: 0o644},
		{name: "preserve-local-content", localContent: "\x00local", localMode: 0o644, conflict: true},
		{name: "preserve-local-readonly", delete: true, localContent: "\x00base", localMode: 0o444, conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			source, job, destination := t.TempDir(), t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "tool"), []byte("\x00base"), 0o644); err != nil {
				t.Fatal(err)
			}
			baseline, err := snapshot.Build(source, []string{"tool"})
			if err != nil {
				t.Fatal(err)
			}
			baseline.Entries[0].Mode = 0o755
			if err := CaptureWorkspaceBaseContext(ctx, source, job, baseline); err != nil {
				t.Fatal(err)
			}
			if tc.delete {
				err = os.Remove(filepath.Join(source, "tool"))
			} else {
				err = os.WriteFile(filepath.Join(source, "tool"), []byte("\x00remote"), 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
			bundle, collected, err := CollectWorkspaceChangesContext(ctx, source, job, baseline, proto.SelectionPolicy{}, 1<<20)
			if err != nil || !collected {
				t.Fatalf("collect = %v, %v", collected, err)
			}
			staged := extractTestBundle(t, job, bundle)
			file := filepath.Join(destination, "tool")
			if err := os.WriteFile(file, []byte(tc.localContent), tc.localMode); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(file, 0o644)
			result, err := Apply(staged, destination, bundle, nil, "test-owner", NewApplyTransaction(), ApplyOptions{})
			if tc.conflict {
				var conflict *MergeConflictError
				if !errors.As(err, &conflict) {
					t.Fatalf("apply = %v, want merge conflict", err)
				}
				content, readErr := os.ReadFile(file)
				info, statErr := os.Stat(file)
				if readErr != nil || statErr != nil || string(content) != tc.localContent || info.Mode().Perm()&0o200 != tc.localMode&0o200 {
					t.Fatalf("conflict changed the local file: %q, %v, %v", content, readErr, statErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := CommitApply(destination, result.Transaction); err != nil {
				t.Fatal(err)
			}
			if tc.delete {
				if _, err := os.Stat(file); !os.IsNotExist(err) {
					t.Fatalf("deleted file stat = %v", err)
				}
			} else if content, err := os.ReadFile(file); err != nil || string(content) != "\x00remote" {
				t.Fatalf("applied content = %q, %v", content, err)
			}
		})
	}
}
