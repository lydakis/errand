package snapshotindex

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

var factories = []struct {
	name  string
	build func([]proto.ManifestEntry) Index
}{
	{"flat", NewFlat},
	{"partitioned", NewPartitioned},
	{"tree", NewTree},
}

func fixture(count int) []proto.ManifestEntry {
	entries := make([]proto.ManifestEntry, count)
	for i := range entries {
		entries[i] = proto.ManifestEntry{Path: fmt.Sprintf("file-%06d", i), Type: proto.EntryFile, Mode: 0644, Size: 1024, SHA256: fmt.Sprintf("%064x", i+1)}
	}
	return entries
}

// The map oracle is intentionally independent of the ordered merge used by the
// flat prototype. Check each representation against it, including change order.
func oracle(entries []proto.ManifestEntry, edits []Edit) []proto.ManifestEntry {
	m := make(map[string]proto.ManifestEntry)
	for _, entry := range entries {
		m[entry.Path] = entry
	}
	for _, edit := range edits {
		if edit.Delete {
			delete(m, edit.Entry.Path)
		} else {
			m[edit.Entry.Path] = edit.Entry
		}
	}
	result := make([]proto.ManifestEntry, 0, len(m))
	for _, entry := range m {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result
}

func TestIndexContract(t *testing.T) {
	for _, factory := range factories {
		t.Run(factory.name, func(t *testing.T) {
			entries := fixture(300)
			index := factory.build(entries)
			rng := rand.New(rand.NewPCG(27, 42))
			for step := range 200 {
				previous, previousDigest := index, index.Digest()
				before := slices.Clone(entries)
				// Multiple unique edits deliberately arrive in unsorted order.
				var edits []Edit
				for _, key := range rng.Perm(400)[:1+step%17] {
					entry := proto.ManifestEntry{Path: fmt.Sprintf("file-%06d", key), Type: proto.EntryFile, Mode: 0644, Size: int64(step + 1), SHA256: fmt.Sprintf("%064x", step+1)}
					switch rng.IntN(4) {
					case 0:
						edits = append(edits, Edit{Entry: entry, Delete: true})
					case 1:
						entry.Mode = 0755
						edits = append(edits, Edit{Entry: entry})
					case 2:
						entry.Type, entry.Size, entry.SHA256, entry.Target = proto.EntrySymlink, 0, "", fmt.Sprintf("target-%d", step)
						edits = append(edits, Edit{Entry: entry})
					default:
						edits = append(edits, Edit{Entry: entry})
					}
				}
				entries = oracle(entries, edits)
				index = index.Update(edits)
				if !slices.Equal(index.Manifest().Entries, entries) {
					t.Fatalf("step %d: wrong snapshot", step)
				}
				if !slices.Equal(previous.Manifest().Entries, before) || previous.Digest() != previousDigest {
					t.Fatal("update changed old snapshot")
				}
				fresh := factory.build(entries)
				if fresh.Digest() != index.Digest() {
					t.Fatalf("step %d: digest depends on update history", step)
				}
				if len(index.Diff(fresh)) != 0 {
					t.Fatal("equivalent independently built snapshots differ")
				}
				got := previous.Diff(index)
				if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i].Entry.Path < got[j].Entry.Path }) {
					t.Fatal("unsorted diff")
				}
				if !slices.Equal(oracle(before, got), entries) {
					t.Fatal("diff does not reconstruct snapshot")
				}
				if !reflect.DeepEqual(got, NewFlat(before).Diff(NewFlat(entries))) {
					t.Fatal("diff includes incorrect or redundant edits")
				}
				if !slices.Equal(oracle(entries, index.Diff(previous)), before) {
					t.Fatal("reverse diff does not reconstruct snapshot")
				}
			}
		})
	}
}

func TestIndexOwnershipAndStructuralChanges(t *testing.T) {
	for _, factory := range factories {
		t.Run(factory.name, func(t *testing.T) {
			input := []proto.ManifestEntry{
				{Path: "a", Type: proto.EntryDir, Mode: 0755},
				{Path: "a/child", Type: proto.EntrySymlink, Mode: 0777, Target: "../b"},
				{Path: "b", Type: proto.EntryFile, Mode: 0644, Size: 1, SHA256: fmt.Sprintf("%064x", 1)},
			}
			want := slices.Clone(input)
			index := factory.build(input)
			input[0].Path = "changed"
			output := index.Manifest()
			output.Entries[0].Mode = 0
			if !slices.Equal(index.Manifest().Entries, want) {
				t.Fatal("manifest aliases caller slice")
			}
			edits := []Edit{{Entry: want[1], Delete: true}, {Entry: proto.ManifestEntry{Path: "a", Type: proto.EntrySymlink, Mode: 0777, Target: "b"}}}
			next := index.Update(edits)
			expected := oracle(want, edits)
			edits[1].Entry.Target = "mutated"
			if !slices.Equal(next.Manifest().Entries, expected) {
				t.Fatal("edits alias snapshot")
			}
			if !slices.Equal(index.Manifest().Entries, want) {
				t.Fatal("structural edit mutated base")
			}
			var remove []Edit
			for _, entry := range expected {
				remove = append(remove, Edit{Entry: entry, Delete: true})
			}
			empty := next.Update(remove)
			if len(empty.Manifest().Entries) != 0 || empty.Digest() != factory.build(nil).Digest() {
				t.Fatal("delete-to-empty is not canonical")
			}
			if len(empty.Diff(factory.build(nil))) != 0 {
				t.Fatal("empty diff")
			}
			if !slices.Equal(oracle(nil, empty.Diff(index)), want) {
				t.Fatal("creation diff failed")
			}
		})
	}
}

// Every field participates in identity and diff. A content hash alone must not
// hide executable-bit changes, symlink targets, or directory replacement.
func TestIndexMetadataIdentity(t *testing.T) {
	base := fixture(1)[0]
	variants := []proto.ManifestEntry{base, base, base, base, base, base}
	variants[0].Mode = 0755
	variants[1].Size++
	variants[2].SHA256 = fmt.Sprintf("%064x", 2)
	variants[3] = proto.ManifestEntry{Path: base.Path, Type: proto.EntryDir, Mode: 0755}
	variants[4] = proto.ManifestEntry{Path: base.Path, Type: proto.EntrySymlink, Mode: 0777, Target: "one"}
	variants[5] = proto.ManifestEntry{Path: base.Path, Type: proto.EntrySymlink, Mode: 0777, Target: "two"}
	for _, factory := range factories {
		t.Run(factory.name, func(t *testing.T) {
			seen := map[[32]byte]bool{}
			index := factory.build([]proto.ManifestEntry{base})
			seen[index.Digest()] = true
			for _, variant := range variants {
				next := index.Update([]Edit{{Entry: variant}})
				if seen[next.Digest()] {
					t.Fatal("metadata change missing from identity")
				}
				seen[next.Digest()] = true
				if len(index.Diff(next)) != 1 {
					t.Fatal("metadata change missing from diff")
				}
			}
		})
	}
}

func TestIndexStructuralDiffIndependentTrees(t *testing.T) {
	entries := fixture(2048)
	var sparse, disjoint []proto.ManifestEntry
	for i, entry := range entries {
		if i%2 == 0 {
			sparse = append(sparse, entry)
		}
		entry.Path = "renamed-" + entry.Path
		disjoint = append(disjoint, entry)
	}
	withoutRoot := oracle(entries, []Edit{{Entry: NewTree(entries).(*tree).root.entry, Delete: true}})
	for _, factory := range factories {
		t.Run(factory.name, func(t *testing.T) {
			base := factory.build(entries)
			for _, after := range [][]proto.ManifestEntry{nil, sparse, disjoint, withoutRoot} {
				next := factory.build(after) // No shared pointers or update history.
				got := base.Diff(next)
				want := NewFlat(entries).Diff(NewFlat(after))
				if !reflect.DeepEqual(got, want) {
					t.Fatal("structural diff differs from reference")
				}
				if base.Update(got).Digest() != next.Digest() || next.Update(next.Diff(base)).Digest() != base.Digest() {
					t.Fatal("structural edits do not reproduce identities")
				}
			}
		})
	}
}

func TestIndexLongMetadata(t *testing.T) {
	// Exercise variable-size metadata beyond the hash encoder's stack buffer.
	entry := proto.ManifestEntry{Path: strings.Repeat("dir/", 160) + "file", Type: proto.EntrySymlink, Mode: 0777, Target: strings.Repeat("target", 200)}
	for _, factory := range factories {
		t.Run(factory.name, func(t *testing.T) {
			base := factory.build([]proto.ManifestEntry{entry})
			changed := entry
			changed.Target += "!"
			next := base.Update([]Edit{{Entry: changed}})
			if base.Digest() == next.Digest() || next.Digest() != factory.build([]proto.ManifestEntry{changed}).Digest() {
				t.Fatal("long metadata lost in identity")
			}
			if !slices.Equal(next.Manifest().Entries, []proto.ManifestEntry{changed}) || !reflect.DeepEqual(base.Diff(next), []Edit{{Entry: changed}}) {
				t.Fatal("long metadata lost in update or diff")
			}
		})
	}
}

func TestTreeBuildWorkerIndependence(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })
	for _, count := range []int{0, 1, 511, 512, 1000, 4096} {
		entries := fixture(count)
		runtime.GOMAXPROCS(1)
		serial := NewTree(entries)
		for _, workers := range []int{2, 4} {
			runtime.GOMAXPROCS(workers)
			parallel := NewTree(entries)
			if parallel.Digest() != serial.Digest() || !slices.Equal(parallel.Manifest().Entries, entries) {
				t.Fatalf("%d entries with %d workers: construction changed snapshot", count, workers)
			}
		}
	}
}
