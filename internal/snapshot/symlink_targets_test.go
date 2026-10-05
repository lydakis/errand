package snapshot

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSymlinkTargetsPackOnWindows(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/file", "content")
	targets := map[string]string{"link": "src/file", "src/link": "../src/file", "dangling": "missing/file"}
	if runtime.GOOS != "windows" {
		// Backslashes are literal filename characters on Unix.
		targets["literal"] = `src\file`
	}
	paths := []string{"src", "src/file"}
	for name, target := range targets {
		if err := os.Symlink(target, filepath.Join(root, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}
	m, err := Build(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range m.Entries {
		if target, ok := targets[e.Path]; ok && e.Target != target {
			t.Fatalf("manifest target for %s = %q, want %q", e.Path, e.Target, target)
		}
	}
	var packed bytes.Buffer
	if err := Pack(&packed, root, m); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(&packed)
	links := 0
	for {
		hdr, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeSymlink {
			if target, ok := targets[hdr.Name]; !ok || hdr.Linkname != target {
				t.Fatalf("archive target for %s = %q, want %q", hdr.Name, hdr.Linkname, target)
			}
			links++
		}
	}
	if links != len(targets) {
		t.Fatalf("packed %d symlinks, want %d", links, len(targets))
	}
}
