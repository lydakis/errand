package proto

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"testing"
)

func legacyRootHash(m Manifest) string {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestManifestJSONMatchesEncodingJSON(t *testing.T) {
	strs := []string{"", "a", "dir/file.txt", "quote\"", "back\\slash", "<html>&", "tab\t", "nl\n", "\x00\x1f",
		"é", "日本", "  ", "bad\xffutf8", "\xed\xa0\x80", "emoji😀", "\x7f"}
	cases := []Manifest{{}, {Entries: []ManifestEntry{}}}
	for _, s := range strs {
		cases = append(cases, Manifest{Entries: []ManifestEntry{
			{Path: s, Type: EntryFile, Mode: 0o644, Size: 3, SHA256: s, Target: s},
			{Path: "x" + s, Type: s, Mode: 0},
		}})
	}
	r := rand.New(rand.NewPCG(1, 2))
	for n := 0; n < 200; n++ {
		var m Manifest
		for i := r.IntN(20); i > 0; i-- {
			b := make([]byte, r.IntN(12))
			for j := range b {
				b[j] = byte(r.IntN(256))
			}
			m.Entries = append(m.Entries, ManifestEntry{Path: string(b), Type: []string{EntryFile, EntryDir, "symlink"}[r.IntN(3)],
				Mode: r.Uint32(), Size: r.Int64() - r.Int64(), SHA256: fmt.Sprintf("%x", b), Target: string(b[:len(b)/2])})
		}
		cases = append(cases, m)
	}
	for i, m := range cases {
		want, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var got bytes.Buffer
		writeManifestJSON(&got, m)
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("case %d: encoding differs\n got %q\nwant %q", i, got.Bytes(), want)
		}
		if m.RootHash() != legacyRootHash(m) {
			t.Fatalf("case %d: root hash differs", i)
		}
	}
}

func benchmarkManifest(n int) Manifest {
	m := Manifest{Entries: make([]ManifestEntry, n)}
	for i := range m.Entries {
		m.Entries[i] = ManifestEntry{Path: fmt.Sprintf("package-%03d/file-%05d.txt", i/100, i), Type: EntryFile, Mode: 0o644,
			Size: 1024, SHA256: fmt.Sprintf("%064x", i)}
	}
	return m
}

func BenchmarkManifestRootHash10K(b *testing.B) {
	m := benchmarkManifest(10000)
	b.Run("stream", func(b *testing.B) {
		for b.Loop() {
			m.RootHash()
		}
	})
	b.Run("json-marshal", func(b *testing.B) {
		for b.Loop() {
			legacyRootHash(m)
		}
	})
}
