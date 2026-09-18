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
	for _, name := range []string{"success", "first-member-failure", "second-member-failure", "barrier-failure"} {
		t.Run(name, func(t *testing.T) {
			target, bundle, staged := groupFixturePaths(t, []string{"x/a", "y/b", "z/c"})
			expected := map[fsidentity.Identity]bool{}
			for _, parent := range []string{"x", "y", "z"} {
				id, _, err := fsidentity.Lstat(filepath.Join(target.Root, parent))
				if err != nil {
					t.Fatal(err)
				}
				expected[id] = false
			}
			injected := errors.New("parent publication failed")
			publications := 0
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
					wantKind := applyMemberSync
					if publications == len(expected)-1 {
						wantKind = applyBarrierSync
					}
					if seen, ok := expected[id]; !ok || seen || kind != wantKind {
						t.Fatalf("wrong parent publication: %v %s", id, kind)
					}
					expected[id] = true
					publications++
					if (name == "first-member-failure" && publications == 1) ||
						(name == "second-member-failure" && publications == 2) ||
						(name == "barrier-failure" && kind == applyBarrierSync) {
						return injected
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
			if name != "success" {
				if !errors.Is(err, injected) || committed {
					t.Fatalf("failed publication committed: %v %v", committed, err)
				}
			} else if err != nil || !committed {
				t.Fatalf("apply: %v %v", committed, err)
			}
			for _, p := range bundle.Paths {
				prefix := "after-"
				if name != "success" {
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

func TestGroupedApplyRequiresTransactionDevice(t *testing.T) {
	target, bundle, staged := groupFixturePaths(t, []string{"x/a", "y/b"})
	destination, err := openApplyDestination(target.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	_, inputs, err := captureApplyInputs(destination, bundle, bundle.Paths)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := makeTreeAccessible(filepath.Join(staged, "remote"))
	if err != nil {
		t.Fatal(err)
	}
	defer merged.restore()
	identity, _, err := fsidentity.Lstat(target.Root)
	if err != nil {
		t.Fatal(err)
	}
	journal := applyJournal{TransactionIdentity: identity}
	for _, p := range bundle.Paths {
		journal.Items = append(journal.Items, applyJournalItem{Path: p})
	}
	if group, reason := planApplyFileGroup(destination, journal, inputs, merged, nil); group == nil {
		t.Fatalf("same-device group refused: %s", reason)
	}
	input := inputs["y/b"]
	input.ancestor.identity.Device++
	inputs["y/b"] = input
	if group, reason := planApplyFileGroup(destination, journal, inputs, merged, nil); group != nil || reason != "cross-device" {
		t.Fatalf("cross-device group admitted: %v %s", group, reason)
	}
}
