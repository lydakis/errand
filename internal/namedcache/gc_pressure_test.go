package namedcache

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestGCIgnoresTemporaryDescriptorExhaustion(t *testing.T) {
	if os.Getenv("ERRAND_GC_FD_PRESSURE_CHILD") != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(executable, "-test.run=^TestGCIgnoresTemporaryDescriptorExhaustion$")
		cmd.Env = append(os.Environ(), "ERRAND_GC_FD_PRESSURE_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("resource pressure regression: %v\n%s", err, output)
		}
		return
	}
	s := openTestStore(t, t.TempDir(), 1<<20)
	key, id := Key{"owner", "project", "deep-tree"}, proto.NewULID()
	data, err := s.AcquireTree(t.Context(), key, id)
	if err != nil {
		t.Fatal(err)
	}
	deepest := data
	for range 90 {
		deepest = filepath.Join(deepest, "d")
	}
	if err := os.MkdirAll(deepest, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deepest, "keep"), []byte("healthy"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseTree(t.Context(), key, id); err != nil {
		t.Fatal(err)
	}
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &original); err != nil {
		t.Fatal(err)
	}
	limited := original
	limited.Cur = 64
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limited); err != nil {
		t.Fatal(err)
	}
	defer syscall.Setrlimit(syscall.RLIMIT_NOFILE, &original)
	// Establish that this limit actually exercises the transient-error path.
	if _, err := s.measureConcurrent(t.Context(), key.hash()+"/data"); err == nil {
		t.Fatal("fixture did not exhaust descriptors")
	}
	result, err := s.GC(t.Context(), false)
	if err != nil || result.Removed != 0 {
		t.Fatalf("healthy cache evicted under descriptor pressure: %+v %v", result, err)
	}
	if raw, err := os.ReadFile(filepath.Join(deepest, "keep")); err != nil || string(raw) != "healthy" {
		t.Fatalf("cache lost: %q %v", raw, err)
	}
}
