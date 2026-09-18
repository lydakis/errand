package changes

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/fsidentity"
)

func TestGroupedApplyPublicationReportsReplacedDirectory(t *testing.T) {
	target, bundle, staged := groupFixturePaths(t, []string{"x/a", "y/b", "y/c"})
	_, err := target.Apply(staged, bundle, nil, ApplyOptions{
		groupCheckpoint: func(event string, _ int) error {
			if event != "install-barrier" {
				return nil
			}
			if err := os.Rename(filepath.Join(target.Root, "y"), filepath.Join(target.Root, "saved-y")); err != nil {
				return err
			}
			return os.Mkdir(filepath.Join(target.Root, "y"), 0700)
		},
	})
	if err == nil || !strings.Contains(err.Error(), `replaced parent directory "y"`) || strings.Contains(err.Error(), "unused") {
		t.Fatalf("expected real directory diagnostic, got %v", err)
	}
	assertTransferFile(t, target.Root, "x/a", "before-x/a")
	assertTransferFile(t, target.Root, "saved-y/b", "after-y/b")
	if pending, err := WorkspaceHasApplyTransactions(target.Root); err != nil || !pending {
		t.Fatalf("replaced parent must retain recovery evidence: %v %v", pending, err)
	}
}

func TestGroupedApplyPublishesEveryParentBeforeCommit(t *testing.T) {
	for _, failLast := range []bool{false, true} {
		name := "success"
		if failLast {
			name = "last-parent-failure"
		}
		t.Run(name, func(t *testing.T) {
			target, bundle, staged := groupFixturePaths(t, []string{"x/a", "y/b", "y/c"})
			expected := map[fsidentity.Identity]bool{}
			for _, parent := range []string{"x", "y"} {
				id, _, err := fsidentity.Lstat(filepath.Join(target.Root, parent))
				if err != nil {
					t.Fatal(err)
				}
				expected[id] = false
			}
			injected := errors.New("last parent publication failed")
			committed := false
			_, err := target.Apply(staged, bundle, nil, ApplyOptions{
				syncCheckpoint: func(role string, kind applySyncKind, dir *os.File) error {
					if role != "install-parent" {
						return nil
					}
					info, err := dir.Stat()
					if err != nil {
						return err
					}
					id, err := fsidentity.FromInfo(info)
					if err != nil {
						return err
					}
					if _, ok := expected[id]; !ok || kind != applyBarrierSync {
						t.Fatalf("wrong parent publication: %v %s", id, kind)
					}
					expected[id] = true
					if failLast {
						all := true
						for _, seen := range expected {
							all = all && seen
						}
						if all {
							return injected
						}
					}
					return nil
				},
				groupCheckpoint: func(event string, _ int) error {
					if event == "committed" {
						committed = true
						for id, seen := range expected {
							if !seen {
								t.Fatalf("committed before publishing %v", id)
							}
						}
					}
					return nil
				},
			})
			if failLast {
				if !errors.Is(err, injected) || committed {
					t.Fatalf("failed publication committed: %v %v", committed, err)
				}
			} else if err != nil || !committed {
				t.Fatalf("apply: %v %v", committed, err)
			}
			for _, p := range bundle.Paths {
				prefix := "after-"
				if failLast {
					prefix = "before-"
				}
				assertTransferFile(t, target.Root, p, prefix+p)
			}
			if pending, err := WorkspaceHasApplyTransactions(target.Root); err != nil || pending {
				t.Fatalf("pending: %v %v", pending, err)
			}
		})
	}
}
