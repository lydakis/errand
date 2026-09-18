package changes

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func groupFixture(t *testing.T) (TransferTarget, proto.ChangeBundle, string) {
	return groupFixturePaths(t, []string{"a", "b", "c"})
}

func groupFixturePaths(t *testing.T, paths []string) (TransferTarget, proto.ChangeBundle, string) {
	t.Helper()
	root, staged := t.TempDir(), t.TempDir()
	base, remote := filepath.Join(staged, "base"), filepath.Join(staged, "remote")
	for _, dir := range []string{base, remote} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	selection := append([]string(nil), paths...)
	parents := map[string]bool{}
	for _, name := range paths {
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			if !parents[parent] {
				selection = append(selection, parent)
				parents[parent] = true
			}
		}
		for _, dir := range []string{root, base, remote} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
				t.Fatal(err)
			}
			value := "before-" + name
			if dir == remote {
				value = "after-" + name
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	b, err := snapshot.Build(base, selection)
	if err != nil {
		t.Fatal(err)
	}
	r, err := snapshot.Build(remote, selection)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := PrepareSourceDelta(t.Context(), b, r, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return transferTarget(t, root), bundle, staged
}

func TestGroupedApplyPhaseOrdering(t *testing.T) {
	target, bundle, staged := groupFixture(t)
	var events []string
	result, err := target.Apply(staged, bundle, nil, ApplyOptions{groupCheckpoint: func(event string, i int) error {
		events = append(events, fmt.Sprintf("%s:%d", event, i))
		if event == "backups-durable" {
			for j, name := range bundle.Paths {
				if j == i {
					if _, err := os.Lstat(filepath.Join(target.Root, name)); !os.IsNotExist(err) {
						t.Fatalf("installed before backup barrier: %s", name)
					}
				} else {
					want := "before-" + name
					if j < i {
						want = "after-" + name
					}
					assertTransferFile(t, target.Root, name, want)
				}
			}
		}
		return nil
	}})
	if err != nil || !reflect.DeepEqual(result.Applied, bundle.Paths) {
		t.Fatalf("apply: %+v %v", result, err)
	}
	want := []string{"staged:-1", "intent:-1"}
	for i := range 3 {
		for _, event := range []string{"before-backup", "backup-rename", "backup-barrier", "backups-durable", "install-rename"} {
			want = append(want, fmt.Sprintf("%s:%d", event, i))
		}
	}
	want = append(want, "install-barrier:-1", "installed:-1", "committed:-1")
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("phase order = %v; want %v", events, want)
	}
}

func TestGroupedApplyFailurePreservesUntouchedEdit(t *testing.T) {
	target, bundle, staged := groupFixture(t)
	injected := errors.New("stop before second backup")
	_, err := target.Apply(staged, bundle, nil, ApplyOptions{groupCheckpoint: func(event string, i int) error {
		if event == "before-backup" && i == 1 {
			if err := os.WriteFile(filepath.Join(target.Root, "b"), []byte("later"), 0600); err != nil {
				t.Fatal(err)
			}
			return injected
		}
		return nil
	}})
	if !errors.Is(err, injected) {
		t.Fatalf("missing injected failure: %v", err)
	}
	for _, name := range bundle.Paths {
		want := "before-" + name
		if name == "b" {
			want = "later"
		}
		assertTransferFile(t, target.Root, name, want)
	}
	if pending, err := WorkspaceHasApplyTransactions(target.Root); err != nil || pending {
		t.Fatalf("unnecessary retained recovery: %v %v", pending, err)
	}
}

func TestGroupedApplyBoundariesPreserveModesAndSelection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		paths   []string
		grouped bool
	}{
		{"single", []string{"a"}, false},
		{"nested", []string{"dir/a", "dir/b"}, true},
		{"different-parents", []string{"x/a", "y/b"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, bundle, staged := groupFixturePaths(t, tc.paths)
			seen := false
			_, err := target.Apply(staged, bundle, nil, ApplyOptions{groupCheckpoint: func(string, int) error { seen = true; return nil }})
			if err != nil || seen != tc.grouped {
				t.Fatalf("apply grouped=%v want %v: %v", seen, tc.grouped, err)
			}
			for _, name := range tc.paths {
				assertTransferFile(t, target.Root, name, "after-"+name)
			}
		})
	}
	for _, mode := range []os.FileMode{0400, 0000} {
		t.Run(fmt.Sprintf("mode-%o", mode), func(t *testing.T) {
			target, bundle, staged := groupFixture(t)
			r, err := snapshot.Build(filepath.Join(staged, "remote"), bundle.Paths)
			if err != nil {
				t.Fatal(err)
			}
			for i := range r.Entries {
				if r.Entries[i].Path == "a" {
					r.Entries[i].Mode = uint32(mode)
				}
			}
			if err := os.Chmod(filepath.Join(staged, "remote", "a"), mode); err != nil {
				t.Fatal(err)
			}

			bundle, err = PrepareSourceDelta(t.Context(), bundle.BaseManifest, r, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			seen := false
			_, err = target.Apply(staged, bundle, nil, ApplyOptions{groupCheckpoint: func(string, int) error { seen = true; return nil }})
			if seen != (mode == 0400) {
				t.Fatalf("grouped=%v for mode %o", seen, mode)
			}
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(target.Root, "a"))
			if err != nil || info.Mode().Perm() != mode {
				t.Fatalf("mode: %v %v", info, err)
			}
		})
	}
}

func TestGroupedApplyRefusesLaterInstalledEdit(t *testing.T) {
	target, bundle, staged := groupFixture(t)
	injected := errors.New("interrupt after first install")
	_, err := target.Apply(staged, bundle, nil, ApplyOptions{groupCheckpoint: func(event string, i int) error {
		if event == "before-backup" && i == 1 {
			writeTransferFile(t, target.Root, "a", "later")
			return injected
		}
		return nil
	}})
	if !errors.Is(err, injected) {
		t.Fatal(err)
	}
	assertTransferFile(t, target.Root, "a", "later")
	if pending, err := WorkspaceHasApplyTransactions(target.Root); err != nil || !pending {
		t.Fatalf("lost recovery data: %v %v", pending, err)
	}
	if _, err := target.Apply(staged, bundle, nil, ApplyOptions{}); err == nil {
		t.Fatal("retry overwrote later edit")
	}
	assertTransferFile(t, target.Root, "a", "later")
}

func TestGroupedApplyErrorsRollbackAtEachBoundary(t *testing.T) {
	for _, event := range []string{"staged", "intent", "backup-rename", "backup-barrier", "backups-durable", "install-rename", "install-barrier", "installed"} {
		t.Run(event, func(t *testing.T) {
			target, bundle, staged := groupFixture(t)
			injected := errors.New("injected publication failure")
			_, err := target.Apply(staged, bundle, nil, ApplyOptions{groupCheckpoint: func(at string, i int) error {
				if at == event && (i == -1 || i == 1) {
					return injected
				}
				return nil
			}})
			if !errors.Is(err, injected) {
				t.Fatalf("failure: %v", err)
			}
			for _, name := range bundle.Paths {
				assertTransferFile(t, target.Root, name, "before-"+name)
			}
			if pending, err := WorkspaceHasApplyTransactions(target.Root); err != nil || pending {
				t.Fatalf("incomplete rollback: %v %v", pending, err)
			}
		})
	}
}

func TestGroupedApplyRefusesReplacedParent(t *testing.T) {
	target, bundle, staged := groupFixturePaths(t, []string{"dir/a", "dir/b"})
	_, err := target.Apply(staged, bundle, nil, ApplyOptions{groupCheckpoint: func(event string, i int) error {
		if event == "before-backup" && i == 1 {
			if err := os.Rename(filepath.Join(target.Root, "dir"), filepath.Join(target.Root, "old-dir")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(target.Root, "dir"), 0700); err != nil {
				t.Fatal(err)
			}
			writeTransferFile(t, target.Root, "dir/b", "replacement")
		}
		return nil
	}})
	if err == nil {
		t.Fatal("accepted replaced parent")
	}
	assertTransferFile(t, target.Root, "dir/b", "replacement")
	assertTransferFile(t, target.Root, "old-dir/b", "before-dir/b")
	if pending, err := WorkspaceHasApplyTransactions(target.Root); err != nil || !pending {
		t.Fatalf("lost recovery data: %v %v", pending, err)
	}
}

func TestCopyToRootSynchronizesFinalMode(t *testing.T) {
	source, dest := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "value"), []byte("value"), 0400); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	injected := errors.New("file sync failed")
	err = copyPathToRootWithSync(source, "value", root, "value", func(f *os.File) error {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.Mode().Perm() != 0400 {
			t.Fatalf("synchronized temporary mode %o", info.Mode().Perm())
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatal(err)
	}
}

func TestGroupedRecoveryRejectsInsufficientEvidence(t *testing.T) {
	for _, damage := range []string{"truncated-journal", "foreign-owner", "committed-group", "corrupt-value", "missing-original", "missing-both"} {
		t.Run(damage, func(t *testing.T) {
			target, bundle, staged := groupFixture(t)
			crashGroupedApply(t, target, bundle, staged, "intent", -1)
			var state transferApplyState
			raw, err := os.ReadFile(target.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			journalFile := filepath.Join(target.Root, state.Pending, applyJournalFile)
			raw, err = os.ReadFile(journalFile)
			if err != nil {
				t.Fatal(err)
			}
			var j applyJournal
			if err := json.Unmarshal(raw, &j); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "truncated-journal":
				raw = raw[:len(raw)/2]
			case "foreign-owner":
				j.Owner = "another owner"
				raw, err = json.Marshal(j)
			case "committed-group":
				j.Phase = applyPhaseCommitted
				raw, err = json.Marshal(j)
			case "corrupt-value":
				err = os.WriteFile(filepath.Join(target.Root, state.Pending, "000000", "value"), []byte("corrupt"), 0600)
			case "missing-both":
				if err := os.Remove(filepath.Join(target.Root, state.Pending, "000000", "value")); err != nil {
					t.Fatal(err)
				}
				fallthrough
			case "missing-original":
				err = os.Remove(filepath.Join(target.Root, "a"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(journalFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := target.Apply(staged, bundle, nil, ApplyOptions{}); err == nil {
				t.Fatal("accepted damaged recovery evidence")
			}
			assertTransferFile(t, target.Root, "b", "before-b")
			if _, err := os.Stat(filepath.Join(target.Root, state.Pending)); err != nil {
				t.Fatalf("discarded recovery: %v", err)
			}
		})
	}
}

func TestGroupedApplyCrashHelper(t *testing.T) {
	file := os.Getenv("ERRAND_GROUP_CRASH_INPUT")
	if file == "" {
		t.Skip("subprocess helper")
	}
	var input struct {
		Target        TransferTarget
		Bundle        proto.ChangeBundle
		Staged, Event string
		Index         int
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	_, err = input.Target.Apply(input.Staged, input.Bundle, nil, ApplyOptions{groupCheckpoint: func(event string, i int) error {
		if event == input.Event && i == input.Index {
			os.Exit(77)
		}
		return nil
	}})
	t.Fatalf("crash boundary not reached: %v", err)
}

func TestGroupedApplyCrashRecovery(t *testing.T) {
	cases := []struct {
		event string
		index int
	}{{"staged", -1}, {"intent", -1}, {"backup-rename", 0}, {"backup-rename", 1}, {"backup-barrier", 1}, {"backups-durable", 1}, {"install-rename", 0}, {"install-rename", 1}, {"install-barrier", -1}, {"installed", -1}, {"committed", -1}}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s-%d", tc.event, tc.index), func(t *testing.T) {
			target, bundle, staged := groupFixture(t)
			crashGroupedApply(t, target, bundle, staged, tc.event, tc.index)
			if tc.event != "committed" {
				var state transferApplyState
				raw, err := os.ReadFile(target.StatePath)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &state); err != nil {
					t.Fatal(err)
				}
				d, err := openApplyDestination(target.Root)
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				if err := target.recoverTransfer(d, &state, func() error { persistTransferState(t, target, state); return nil }); err != nil {
					t.Fatal(err)
				}
				for _, name := range bundle.Paths {
					assertTransferFile(t, target.Root, name, "before-"+name)
				}
			}
			first, err := target.Apply(staged, bundle, nil, ApplyOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range bundle.Paths {
				assertTransferFile(t, target.Root, name, "after-"+name)
			}
			writeTransferFile(t, target.Root, "a", "later")
			retry, err := target.Apply(staged, bundle, nil, ApplyOptions{})
			if err != nil || !reflect.DeepEqual(first, retry) {
				t.Fatalf("retry: %+v %v", retry, err)
			}
			assertTransferFile(t, target.Root, "a", "later")
		})
	}
}

func crashGroupedApply(t *testing.T, target TransferTarget, bundle proto.ChangeBundle, staged, event string, index int) {
	t.Helper()
	input := struct {
		Target        TransferTarget
		Bundle        proto.ChangeBundle
		Staged, Event string
		Index         int
	}{target, bundle, staged, event, index}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestGroupedApplyCrashHelper$")
	cmd.Env = append(os.Environ(), "ERRAND_GROUP_CRASH_INPUT="+file)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 77 {
		t.Fatalf("child: %v %s", err, output)
	}
}
