package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestServiceExecutablePreservesStableSymlinkAcrossUpgrade(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, "errand-0.1.0")
	new := filepath.Join(root, "errand-0.2.0")
	for _, path := range []string{old, new} {
		if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stable := filepath.Join(root, "errand")
	if err := os.Symlink(old, stable); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	servicePath := serviceExecutable(old, "errand")
	if servicePath != stable {
		t.Fatalf("service points to versioned binary %q, want %q", servicePath, stable)
	}
	if err := os.Remove(stable); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(new, stable); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(servicePath)
	if err != nil || resolved != new {
		t.Fatalf("service did not follow upgrade: %q, %v", resolved, err)
	}
	if got := serviceExecutable(new, stable); got != servicePath {
		t.Fatalf("setup after upgrade would change the service path: %q", got)
	}
	if got := serviceExecutable(old, stable); got != old {
		t.Fatalf("different invocation target replaced the running executable: %q", got)
	}
	if got := serviceExecutable(old, filepath.Join(root, "missing")); got != old {
		t.Fatalf("missing invocation target replaced the running executable: %q", got)
	}
}
