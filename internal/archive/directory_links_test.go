package archive

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

// A workspace push uploads only changed entries. On Windows, a new link to a
// directory that already exists must still be created as a directory link,
// which only the complete source manifest can tell.
func TestExtractWithTypesPartialDirectoryLinks(t *testing.T) {
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "target", "value"), []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := proto.ManifestEntry{Path: "nested/alias", Type: proto.EntrySymlink, Target: "../target", Mode: 0o777}
	complete := proto.Manifest{Entries: []proto.ManifestEntry{
		{Path: "nested", Type: proto.EntryDir, Mode: 0o755},
		link,
		{Path: "target", Type: proto.EntryDir, Mode: 0o755},
		entryFile("target/value", "body"),
	}}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: link.Path, Typeflag: tar.TypeSymlink, Linkname: link.Target})
	tw.Close()

	partial := proto.Manifest{Entries: []proto.ManifestEntry{link}}
	if err := ExtractWith(&buf, dest, partial, 1<<20, ExtractOptions{SymlinkManifest: &complete}); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(dest, "nested", "alias", "value")); err != nil || string(body) != "body" {
		t.Fatalf("read through pushed directory link = %q, %v", body, err)
	}
}
