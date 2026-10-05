package snapshot

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestPackContentWritesTheSameArchiveAsPack(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "shared body")
	writeFile(t, root, "dir/b.txt", "shared body")
	writeFile(t, root, "dir/tool", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(root, "dir/tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	m, err := Build(root, []string{"a.txt", "dir", "dir/b.txt", "dir/tool", "link"})
	if err != nil {
		t.Fatal(err)
	}
	var fromTree, fromContent bytes.Buffer
	if err := Pack(&fromTree, root, m); err != nil {
		t.Fatal(err)
	}
	// The content store holds one body per hash, under names unrelated to paths.
	store := t.TempDir()
	for _, e := range m.Entries {
		if e.Type == proto.EntryFile {
			body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(e.Path)))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(store, e.SHA256), body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	err = PackContent(context.Background(), &fromContent, m, func(e proto.ManifestEntry) (io.ReadCloser, error) {
		return os.Open(filepath.Join(store, e.SHA256))
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fromTree.Bytes(), fromContent.Bytes()) {
		t.Fatal("content archive differs from the tree archive")
	}
}

func TestPackContentRefusesABodyThatDoesNotMatch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "original")
	m, err := Build(root, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"same size": "MUTATED!", "longer": "original plus", "shorter": "orig"} {
		t.Run(name, func(t *testing.T) {
			err := PackContent(context.Background(), io.Discard, m, func(proto.ManifestEntry) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(body)), nil
			})
			if err == nil || !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("PackContent = %v, want a content mismatch", err)
			}
		})
	}
}
