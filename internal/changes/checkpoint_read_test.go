package changes

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

// largeCheckpoint publishes an initial checkpoint of several read chunks and
// returns a function that opens a new request handle sharing reuse.
func largeCheckpoint(t *testing.T, entries int) (func() *TransferCheckpoint, []byte) {
	t.Helper()
	template := checkpointFor(t, transferTarget(t, t.TempDir()))
	template.Reuse = NewCheckpointCache(4, 64<<20)
	request := func() *TransferCheckpoint {
		return &TransferCheckpoint{Root: template.Root, RootID: template.RootID, Owner: template.Owner,
			SourceID: template.SourceID, StatePath: template.StatePath, Reuse: template.Reuse}
	}
	m := proto.Manifest{}
	for i := range entries {
		m.Entries = append(m.Entries, proto.ManifestEntry{Path: fmt.Sprintf("file-%06d.txt", i), Type: proto.EntryFile,
			Mode: 0o644, Size: 100, SHA256: fmt.Sprintf("%064x", i)})
	}
	if _, err := request().Initialize(m); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(template.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 4*recordReadChunk {
		t.Fatalf("checkpoint of %d bytes spans too few read chunks", len(raw))
	}
	return request, raw
}

// A read that finds the retained bytes must not hold a second copy of them.
// Every read still reads and compares the whole record.
func TestCheckpointReadMatchesRetainedBytesWithoutCopying(t *testing.T) {
	request, raw := largeCheckpoint(t, 10000)
	if _, err := request().Base(); err != nil {
		t.Fatal(err)
	}
	const requests = 10
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range requests {
		c := request()
		for range 2 { // retained across requests, then by this request
			if _, err := c.Base(); err != nil {
				t.Fatal(err)
			}
		}
	}
	runtime.ReadMemStats(&after)
	if perRead := (after.TotalAlloc - before.TotalAlloc) / (2 * requests); perRead > uint64(len(raw))/16 {
		t.Fatalf("reading an unchanged %d-byte checkpoint allocated %d bytes per read", len(raw), perRead)
	}
}

// Edits past the first chunk, and a record that only grows or shrinks, are
// read in full and never matched to the retained record, with times restored.
func TestCheckpointReadRefusesEditsPastFirstChunk(t *testing.T) {
	for _, mutation := range []string{"last entry", "trailing newline", "truncated"} {
		t.Run(mutation, func(t *testing.T) {
			request, raw := largeCheckpoint(t, 10000)
			retained := request()
			if _, err := retained.readVersion(); err != nil {
				t.Fatal(err)
			}
			path := retained.StatePath
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			edited := bytes.Clone(raw)
			switch mutation {
			case "last entry":
				i := bytes.LastIndex(edited, []byte(fmt.Sprintf("%064x", 9999)))
				edited[i] = 'f'
			case "trailing newline":
				edited = append(edited, '\n')
			case "truncated":
				edited = edited[:len(edited)-1]
			}
			if err := os.WriteFile(path, edited, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			for name, c := range map[string]*TransferCheckpoint{"request": retained, "new request": request()} {
				record, err := c.readVersion()
				if mutation != "trailing newline" {
					if err == nil {
						t.Fatalf("%s: reused the retained checkpoint after an edit", name)
					}
					continue
				}
				// Valid JSON with other bytes: decoded afresh, never matched.
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if !bytes.Equal(record.raw, edited) {
					t.Fatalf("%s: record does not hold the bytes read", name)
				}
			}
		})
	}
}

// Records that end inside a chunk and exactly at a chunk boundary, so a
// longer file can begin its extra bytes in the same read or in the next one.
func TestReadTransferRecordMatchingComparesEveryByte(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	chunk := recordReadChunk
	for _, size := range []int{3*chunk + 123, 2 * chunk} {
		known := make([]byte, size)
		for i := range known {
			known[i] = byte('a' + i%26)
		}
		flip := func(at int) func([]byte) []byte {
			return func(b []byte) []byte { b[at] ^= 1; return b }
		}
		for name, mutate := range map[string]func([]byte) []byte{
			"same":                  func(b []byte) []byte { return b },
			"first byte":            flip(0),
			"last byte of a chunk":  flip(chunk - 1),
			"first byte of a chunk": flip(chunk),
			"last byte":             flip(size - 1),
			"appended":              func(b []byte) []byte { return append(b, 'x') },
			"appended a chunk":      func(b []byte) []byte { return append(b, known[:chunk]...) },
			"one byte short":        func(b []byte) []byte { return b[:size-1] },
			"one chunk":             func(b []byte) []byte { return b[:chunk] },
			"empty":                 func(b []byte) []byte { return b[:0] },
		} {
			file := mutate(bytes.Clone(known))
			if err := os.WriteFile(filepath.Join(dir, "record"), file, 0o600); err != nil {
				t.Fatal(err)
			}
			raw, same, err := readTransferRecordMatching(root, "record", known)
			if err != nil {
				t.Fatalf("%d bytes, %s: %v", size, name, err)
			}
			if same != bytes.Equal(file, known) {
				t.Fatalf("%d bytes, %s: matched = %t", size, name, same)
			}
			if same && raw != nil || !same && !bytes.Equal(raw, file) {
				t.Fatalf("%d bytes, %s: returned %d bytes for a %d-byte file", size, name, len(raw), len(file))
			}
			if !same && len(raw) > 0 && &raw[0] == &known[0] {
				t.Fatalf("%d bytes, %s: returned bytes alias the retained record", size, name)
			}
		}
	}
}

func TestReadTransferRecordMatchingKeepsRecordChecks(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	known := []byte(`{"version":1}`)
	other := filepath.Join(t.TempDir(), "record")
	if err := os.WriteFile(other, known, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(dir, "link")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, _, err := readTransferRecordMatching(root, "link", known); err == nil {
		t.Fatal("matched a symlink")
	}
	if _, _, err := readTransferRecordMatching(root, "missing", known); !os.IsNotExist(err) {
		t.Fatalf("missing record: %v", err)
	}
	// Past the limit, a record that starts with the retained bytes is refused.
	large, err := os.Create(filepath.Join(dir, "large"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = large.Write(known)
	if err == nil {
		err = large.Truncate(MaxBundleMetadataBytes + 1)
	}
	if err := errors.Join(err, large.Close()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readTransferRecordMatching(root, "large", known); err == nil {
		t.Fatal("read a record past the size limit")
	}
}
