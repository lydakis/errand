package proto

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
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
	cases := []Manifest{{}, {Entries: []ManifestEntry{}}, {Entries: []ManifestEntry{
		{Path: "min", Type: EntryFile, Mode: math.MaxUint32, Size: math.MinInt64},
		{Path: "max", Type: EntryFile, Size: math.MaxInt64, SHA256: "ab"},
		{Path: "neg", Type: EntryFile, Size: -1, Target: "t"},
	}}}
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
		if appended := AppendManifestJSON([]byte("prefix"), m); !bytes.Equal(appended, append([]byte("prefix"), want...)) {
			t.Fatalf("case %d: appended encoding differs\n got %q\nwant %q", i, appended, want)
		}
		checkManifestJSONSize(t, m, want)
		if m.RootHash() != legacyRootHash(m) || ManifestRootHash(want) != m.RootHash() {
			t.Fatalf("case %d: root hash differs", i)
		}
	}
}

// The size is exact when no string is escaped, and never more than the output.
func checkManifestJSONSize(t *testing.T, m Manifest, encoded []byte) {
	t.Helper()
	plain := true
	for _, e := range m.Entries {
		for _, s := range []string{e.Path, e.Type, e.SHA256, e.Target} {
			if q, _ := json.Marshal(s); len(q) != len(s)+2 {
				plain = false
			}
		}
	}
	if size := ManifestJSONSize(m); size > len(encoded) || plain && size != len(encoded) {
		t.Fatalf("size %d for %d encoded bytes (unescaped strings: %t)", size, len(encoded), plain)
	}
}

// Every byte value on its own, and fuzzed strings, in each string field.
func FuzzManifestJSONMatchesEncodingJSON(f *testing.F) {
	for c := range 256 {
		f.Add(string([]byte{byte(c)}))
	}
	for _, s := range []string{"dir/file.txt", "a<b>&c", "\u2028", "\u2029", "é", "bad\xffutf8"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		m := Manifest{Entries: []ManifestEntry{{Path: s, Type: s, Mode: 1, Size: 2, SHA256: s, Target: s}}}
		want, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var got bytes.Buffer
		writeManifestJSON(&got, m)
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("encoding of %q differs\n got %q\nwant %q", s, got.Bytes(), want)
		}
		if appended := AppendManifestJSON(nil, m); !bytes.Equal(appended, want) {
			t.Fatalf("appended encoding of %q differs\n got %q\nwant %q", s, appended, want)
		}
		checkManifestJSONSize(t, m, want)
		want, err = json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if got := AppendJSONString([]byte("x"), s); !bytes.Equal(got, append([]byte("x"), want...)) {
			t.Fatalf("string encoding of %q differs\n got %q\nwant %q", s, got, want)
		}
	})
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
