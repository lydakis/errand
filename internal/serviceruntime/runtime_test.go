package serviceruntime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestPrepareRetainsRunningGenerationAfterInstallationRemoval(t *testing.T) {
	root := t.TempDir()
	installed := filepath.Join(root, "Cellar", "errand")
	if err := os.MkdirAll(filepath.Dir(installed), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(installed, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write("old executable")
	state := filepath.Join(root, "state")
	old, err := Prepare(installed, state)
	if err != nil {
		t.Fatal(err)
	}
	write("new executable")
	next, err := Prepare(installed, state)
	if err != nil {
		t.Fatal(err)
	}
	if old == next {
		t.Fatal("replacement reused running executable path")
	}
	if err := os.RemoveAll(filepath.Dir(installed)); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{old: "old executable", next: "new executable"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("runtime %s: %q / %v", path, got, err)
		}
		again, err := Prepare(path, state)
		if err != nil || again != path {
			t.Fatalf("re-execution must stop at immutable runtime: %s / %v", again, err)
		}
	}
}

func TestPrepareConcurrentAndRefusesDamagedRuntime(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "installed")
	if err := os.WriteFile(source, []byte("executable"), 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := Prepare(source, state); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	target, err := Prepare(source, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("damaged"), 0500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0500); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(source, state); err == nil {
		t.Fatal("accepted damaged runtime")
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, target); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(source, state); err == nil {
		t.Fatal("accepted runtime symlink into installation")
	}
}

func TestPrepareRefusesSharedOrRedirectedRuntimeDirectory(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared", true: "symlink"}[symlink], func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "installed")
			if err := os.WriteFile(source, []byte("executable"), 0700); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "runtime")
			if symlink {
				if err := os.Symlink(t.TempDir(), dir); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Prepare(source, root); err == nil {
				t.Fatal("accepted unsafe runtime directory")
			}
		})
	}
}

func TestPrepareRelativeStateAndReuseWithoutWrites(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("installed", []byte("executable"), 0700); err != nil {
		t.Fatal(err)
	}
	target, err := Prepare("installed", "state")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(target) {
		t.Errorf("runtime path must be absolute: %s", target)
	}
	// A published generation needs no writable staging space, including on
	// the second startup after re-exec. Read-only directories enforce this.
	for _, dir := range []string{filepath.Dir(target), filepath.Dir(filepath.Dir(target))} {
		if err := os.Chmod(dir, 0500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0700) })
	}
	for _, source := range []string{"installed", target} {
		again, err := Prepare(source, "state")
		if err != nil || again != target {
			t.Errorf("reuse %s without writes: %s / %v", source, again, err)
		}
	}
}

func TestReexecAfterInstallationRemoval(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	switch os.Getenv("ERRAND_TEST_REEXEC_PHASE") {
	case "installed":
		source, err := os.Open(executable)
		if err != nil {
			t.Fatal(err)
		}
		defer source.Close()
		target, err := prepare(source, "state")
		if err != nil {
			t.Fatal(err)
		}
		// Reproduce cleanup in the interval between publication and re-exec.
		if err := os.Remove(executable); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ERRAND_TEST_REEXEC_PHASE", "runtime")
		t.Setenv("ERRAND_TEST_REEXEC_PATH", target)
		t.Setenv("ERRAND_TEST_REEXEC_PID", strconv.Itoa(os.Getpid()))
		t.Fatalf("re-exec returned: %v", execPrepared(source, target))
	case "runtime":
		if executable != os.Getenv("ERRAND_TEST_REEXEC_PATH") || strconv.Itoa(os.Getpid()) != os.Getenv("ERRAND_TEST_REEXEC_PID") {
			t.Fatal("re-exec must preserve PID and enter the retained executable")
		}
		// A second startup must recognize its own inode and return normally.
		if err := Reexec("state"); err != nil {
			t.Fatal(err)
		}
		return
	}
	root := t.TempDir()
	installed := filepath.Join(root, "installed")
	body, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, body, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, installed, "-test.run=^TestReexecAfterInstallationRemoval$")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "ERRAND_TEST_REEXEC_PHASE=installed")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("re-exec after cleanup: %v\n%s", err, output)
	}
}
