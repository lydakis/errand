package archive

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func entryFile(path, content string) proto.ManifestEntry {
	sum := sha256.Sum256([]byte(content))
	return proto.ManifestEntry{
		Path: path, Type: proto.EntryFile, Mode: 0o644,
		Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:]),
	}
}

func tarOf(t *testing.T, entries map[string]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(content))
	}
	tw.Close()
	return &buf
}

func TestValidateRejectsUnsafePaths(t *testing.T) {
	cases := []proto.Manifest{
		{Entries: []proto.ManifestEntry{{Path: "/etc/passwd", Type: proto.EntryFile}}},
		{Entries: []proto.ManifestEntry{{Path: "../escape", Type: proto.EntryFile}}},
		{Entries: []proto.ManifestEntry{{Path: "a/../../b", Type: proto.EntryFile}}},
		{Entries: []proto.ManifestEntry{{Path: "a", Type: proto.EntryFile}, {Path: "a", Type: proto.EntryFile}}},
		{Entries: []proto.ManifestEntry{{Path: "l", Type: proto.EntrySymlink, Target: "/etc"}}},
		{Entries: []proto.ManifestEntry{{Path: "l", Type: proto.EntrySymlink, Target: "../.."}}},
		{Entries: []proto.ManifestEntry{
			{Path: "l", Type: proto.EntrySymlink, Target: "sub"},
			{Path: "l/inner", Type: proto.EntryFile},
		}},
	}
	for i, m := range cases {
		if err := Validate(m); err == nil {
			t.Errorf("case %d: expected rejection, got nil", i)
		}
	}
}

func TestValidateAcceptsInternalSymlink(t *testing.T) {
	m := proto.Manifest{Entries: []proto.ManifestEntry{
		{Path: "dir", Type: proto.EntryDir, Mode: 0o755},
		{Path: "dir/link", Type: proto.EntrySymlink, Target: "../real.txt"},
		entryFile("real.txt", "hi"),
	}}
	if err := Validate(m); err != nil {
		t.Fatalf("internal symlink should validate: %v", err)
	}
}

func TestValidateRejectsMalformedEntries(t *testing.T) {
	cases := []proto.Manifest{
		{Entries: []proto.ManifestEntry{{Path: "mystery", Type: "device"}}},
		{Entries: []proto.ManifestEntry{{Path: "file", Type: proto.EntryFile, Size: -1, SHA256: strings.Repeat("0", 64)}}},
		{Entries: []proto.ManifestEntry{{Path: "file", Type: proto.EntryFile, SHA256: "not-a-digest"}}},
		{Entries: []proto.ManifestEntry{entryFile("parent", "x"), entryFile("parent/child", "y")}},
	}
	for i, m := range cases {
		if err := Validate(m); err == nil {
			t.Errorf("case %d: malformed manifest passed validation", i)
		}
	}
}

func TestExtractVerifiesHashes(t *testing.T) {
	m := proto.Manifest{Entries: []proto.ManifestEntry{entryFile("a.txt", "hello")}}
	// Stream carries different content than the manifest promises.
	buf := tarOf(t, map[string]string{"a.txt": "HELLO"})
	err := Extract(buf, t.TempDir(), m, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("expected hash mismatch, got %v", err)
	}
}

func TestExtractRejectsUnmanifestedEntry(t *testing.T) {
	m := proto.Manifest{Entries: []proto.ManifestEntry{entryFile("a.txt", "hello")}}
	buf := tarOf(t, map[string]string{"a.txt": "hello", "sneaky.txt": "x"})
	if err := Extract(buf, t.TempDir(), m, 1<<20); err == nil {
		t.Fatal("expected rejection of entry not in manifest")
	}
}

func TestExtractRejectsAnyManifestEntryMissingFromStream(t *testing.T) {
	m := proto.Manifest{Entries: []proto.ManifestEntry{
		{Path: "empty", Type: proto.EntryDir, Mode: 0o755},
		entryFile("a.txt", "hello"),
	}}
	buf := tarOf(t, map[string]string{"a.txt": "hello"})
	err := Extract(buf, t.TempDir(), m, 1<<20)
	if err == nil || !strings.Contains(err.Error(), `manifest entry "empty" missing`) {
		t.Fatalf("expected missing directory rejection, got %v", err)
	}
}

func TestExtractEnforcesSizeLimit(t *testing.T) {
	content := strings.Repeat("x", 100)
	m := proto.Manifest{Entries: []proto.ManifestEntry{entryFile("big.txt", content)}}
	buf := tarOf(t, map[string]string{"big.txt": content})
	err := Extract(buf, t.TempDir(), m, 50)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size limit error, got %v", err)
	}
}

func TestExtractRoundTrip(t *testing.T) {
	m := proto.Manifest{Entries: []proto.ManifestEntry{
		{Path: "dir", Type: proto.EntryDir, Mode: 0o755},
		entryFile("dir/a.txt", "hello"),
		{Path: "link", Type: proto.EntrySymlink, Target: "dir/a.txt", Mode: 0o777},
	}}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "dir/a.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5})
	tw.Write([]byte("hello"))
	tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "dir/a.txt"})
	tw.Close()

	dest := t.TempDir()
	if err := Extract(&buf, dest, m, 1<<20); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "link")) // through the symlink
	if err != nil || string(got) != "hello" {
		t.Fatalf("round trip failed: %q, %v", got, err)
	}
}

func TestExtractRestoresManifestDirectoryMode(t *testing.T) {
	m := proto.Manifest{Entries: []proto.ManifestEntry{
		{Path: "readonly", Type: proto.EntryDir, Mode: 0o555},
		entryFile("readonly/file.txt", "hello"),
	}}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "readonly/", Typeflag: tar.TypeDir, Mode: 0o555}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "readonly/file.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "readonly"), 0o700) })
	if err := Extract(&buf, dest, m, 1<<20); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dest, "readonly"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o555 {
		t.Fatalf("directory mode = %04o, want 0555", got)
	}
}

func TestExtractRestoresFileModeMaskedByUmask(t *testing.T) {
	e := entryFile("tool", "hello")
	e.Mode = 0o755
	m := proto.Manifest{Entries: []proto.ManifestEntry{e}}
	buf := tarOf(t, map[string]string{"tool": "hello"})
	dest := t.TempDir()
	oldUmask := syscall.Umask(0o077)
	err := Extract(buf, dest, m, 1<<20)
	syscall.Umask(oldUmask)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dest, "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("file mode = %04o, want 0755", got)
	}
}
