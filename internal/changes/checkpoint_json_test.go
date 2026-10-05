package changes

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"

	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
)

func checkCheckpointJSON(t *testing.T, state checkpointState) {
	t.Helper()
	want, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	got, manifest := state.encode()
	if !bytes.Equal(got, want) {
		t.Fatalf("checkpoint encoding differs\n got %q\nwant %q", got, want)
	}
	if root := proto.ManifestRootHash(manifest); root != state.Manifest.RootHash() {
		t.Fatalf("root of the encoded manifest %q = %s, want %s", manifest, root, state.Manifest.RootHash())
	}
}

func TestCheckpointJSONMatchesEncodingJSON(t *testing.T) {
	entries := []proto.ManifestEntry{
		{Path: "dir", Type: proto.EntryDir, Mode: 0o755},
		{Path: "dir/a<b>&\"c\"", Type: proto.EntryFile, Mode: 0o644, Size: 3, SHA256: "ab"},
		{Path: "link", Type: proto.EntrySymlink, Mode: 0o777, Target: "bad\xffutf8"},
	}
	for _, state := range []checkpointState{
		{},
		{CheckpointVersion: CheckpointVersion{Manifest: proto.Manifest{Entries: []proto.ManifestEntry{}}}},
		{Version: 1, Owner: "owner", SourceID: "source", RootID: fsidentity.Identity{Device: math.MaxUint64, Inode: 7},
			InitialRoot: "root", CheckpointVersion: CheckpointVersion{Revision: math.MaxUint64, Manifest: proto.Manifest{Entries: entries}}},
		{Version: -1, Owner: " ", SourceID: "\x00", InitialRoot: "é", LastReceipt: "/state/receipt.json", LastRequest: "request",
			CheckpointVersion: CheckpointVersion{Revision: 1, Manifest: proto.Manifest{Entries: entries}}},
		{LastRequest: "only the request"},
	} {
		checkCheckpointJSON(t, state)
	}
	r := rand.New(rand.NewPCG(3, 4))
	text := func() string {
		b := make([]byte, r.IntN(8))
		for i := range b {
			b[i] = byte(r.IntN(256))
		}
		return string(b)
	}
	for range 200 {
		state := checkpointState{Version: r.Int() - r.Int(), Owner: text(), SourceID: text(),
			RootID: fsidentity.Identity{Device: r.Uint64(), Inode: r.Uint64()}, InitialRoot: text(),
			CheckpointVersion: CheckpointVersion{Revision: r.Uint64()}, LastReceipt: text(), LastRequest: text()}
		for i := r.IntN(10); i > 0; i-- {
			state.Manifest.Entries = append(state.Manifest.Entries, proto.ManifestEntry{Path: text(), Type: text(),
				Mode: r.Uint32(), Size: r.Int64() - r.Int64(), SHA256: text(), Target: text()})
		}
		checkCheckpointJSON(t, state)
	}
}

// Every byte value on its own, and fuzzed strings, in each string field.
func FuzzCheckpointJSONMatchesEncodingJSON(f *testing.F) {
	for c := range 256 {
		f.Add(string([]byte{byte(c)}), uint64(c))
	}
	f.Add("bad\xffutf8", uint64(math.MaxUint64))
	f.Fuzz(func(t *testing.T, s string, n uint64) {
		checkCheckpointJSON(t, checkpointState{Version: int(n), Owner: s, SourceID: s, RootID: fsidentity.Identity{Device: n, Inode: n},
			InitialRoot: s, CheckpointVersion: CheckpointVersion{Revision: n, Manifest: proto.Manifest{Entries: []proto.ManifestEntry{
				{Path: s, Type: s, Mode: uint32(n), Size: int64(n), SHA256: s, Target: s}}}}, LastReceipt: s, LastRequest: s})
	})
}

// encode writes each field by hand, so a new field must be added there too.
func TestCheckpointJSONCoversEveryField(t *testing.T) {
	fields := func(typ reflect.Type) (fields []string) {
		for _, field := range reflect.VisibleFields(typ) {
			if !field.Anonymous {
				fields = append(fields, fmt.Sprintf("%s %s %s", field.Name, field.Type, field.Tag.Get("json")))
			}
		}
		return fields
	}
	for _, c := range []struct {
		typ  reflect.Type
		want []string
	}{
		{reflect.TypeFor[checkpointState](), []string{
			"Version int version",
			"Owner string owner",
			"SourceID string source_identity",
			"RootID fsidentity.Identity destination_identity",
			"InitialRoot string initial_root",
			"Revision uint64 revision",
			"Manifest proto.Manifest manifest",
			"LastReceipt string last_receipt,omitempty",
			"LastRequest string last_request,omitempty",
		}},
		{reflect.TypeFor[fsidentity.Identity](), []string{"Device uint64 device", "Inode uint64 inode"}},
	} {
		if got := fields(c.typ); !slices.Equal(got, c.want) {
			t.Fatalf("%s fields changed; update encode and this list\n got %q\nwant %q", c.typ, got, c.want)
		}
	}
	// A custom encoding in any of these would change what json.Marshal writes.
	for _, typ := range []reflect.Type{reflect.TypeFor[checkpointState](), reflect.TypeFor[CheckpointVersion](),
		reflect.TypeFor[fsidentity.Identity](), reflect.TypeFor[proto.Manifest](), reflect.TypeFor[proto.ManifestEntry]()} {
		for _, typ := range []reflect.Type{typ, reflect.PointerTo(typ)} {
			if typ.Implements(reflect.TypeFor[json.Marshaler]()) || typ.Implements(reflect.TypeFor[encoding.TextMarshaler]()) {
				t.Fatalf("%s has a custom JSON encoding; update encode", typ)
			}
		}
	}
}

// BenchmarkCheckpointEncode encodes a 10K-entry checkpoint, as Advance
// publishes one after every watch save.
func BenchmarkCheckpointEncode(b *testing.B) {
	state := checkpointState{Version: 1, Owner: "owner", SourceID: "source", InitialRoot: (proto.Manifest{}).RootHash(),
		CheckpointVersion: CheckpointVersion{Revision: 1}, LastReceipt: "/state/receipt.json", LastRequest: "request"}
	for i := range 10000 {
		state.Manifest.Entries = append(state.Manifest.Entries, proto.ManifestEntry{Path: fmt.Sprintf("packages/pkg-%03d/src/file-%05d", i/100, i),
			Type: proto.EntryFile, Mode: 0o644, Size: 1024, SHA256: fmt.Sprintf("%064x", i)})
	}
	b.Run("json-marshal", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := json.Marshal(state); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("encode", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			state.encode()
		}
	})
}
