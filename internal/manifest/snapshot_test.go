package manifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

type cancelDuringWork struct {
	context.Context
	cancel    context.CancelFunc
	remaining atomic.Int64
}

func (c *cancelDuringWork) Err() error {
	if c.remaining.Add(-1) == 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestCancelledUpdateDoesNotPoisonSnapshot(t *testing.T) {
	entries := make([]proto.ManifestEntry, 1024)
	for i := range entries {
		entries[i] = file(fmt.Sprintf("file-%04d", i), 1)
	}
	s := mustNew(t, entries)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	during := &cancelDuringWork{Context: ctx, cancel: cancel}
	during.remaining.Store(100)
	edit := make([]Edit, 256)
	for i := range edit {
		edit[i] = Edit{Entry: file(entries[i].Path, 2)}
	}
	if next, err := s.Update(during, edit); next != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("published=%t err=%v", next != nil, err)
	}
	next, err := s.Update(t.Context(), edit)
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(entries)
	for i := range edit {
		want[i] = edit[i].Entry
	}
	if !slices.Equal(export(t, s).Entries, entries) || !slices.Equal(export(t, next).Entries, want) {
		t.Fatal("cancelled build changed subsequent results")
	}
}

func TestIndependentSnapshotsPreserveStructuralAndMetadataDiffs(t *testing.T) {
	base := []proto.ManifestEntry{{Path: "dir", Type: proto.EntryDir, Mode: 0755}, file("dir/child", 1), {Path: "link", Type: proto.EntrySymlink, Target: "dir/child"}}
	current := []proto.ManifestEntry{file("dir", 3), {Path: "link", Type: proto.EntrySymlink, Target: "dir"}}
	a, b := mustNew(t, base), mustNew(t, current)
	want := []Edit{{Entry: current[0]}, {Entry: base[1], Delete: true}, {Entry: current[1]}}
	got, err := a.Diff(t.Context(), b)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("diff=%v want=%v err=%v", got, want, err)
	}
	result, err := a.Update(t.Context(), got)
	if err != nil || !slices.Equal(export(t, result).Entries, current) {
		t.Fatalf("reconstruction: %v", err)
	}
}

func TestDiffObservesIndividualMetadataChanges(t *testing.T) {
	base := file("a", 1)
	mode, size, hash := base, base, base
	mode.Mode = 0600
	size.Size = 2
	hash.SHA256 = fmt.Sprintf("%064x", 2)
	dir := proto.ManifestEntry{Path: "a", Type: proto.EntryDir, Mode: 0755}
	dirMode := dir
	dirMode.Mode = 0700
	link := proto.ManifestEntry{Path: "a", Type: proto.EntrySymlink, Target: "b"}
	linkTarget := link
	linkTarget.Target = "c"
	for _, pair := range [][2]proto.ManifestEntry{{base, mode}, {base, size}, {base, hash}, {dir, dirMode}, {link, linkTarget}} {
		before := mustNew(t, []proto.ManifestEntry{pair[0]})
		if _, err := before.tree(t.Context()); err != nil {
			t.Fatal(err)
		}
		after, err := before.Update(t.Context(), []Edit{{Entry: pair[1]}})
		if err != nil {
			t.Fatal(err)
		}
		diff, err := before.Diff(t.Context(), after)
		if err != nil || len(diff) != 1 || diff[0].Delete || diff[0].Entry != pair[1] {
			t.Fatalf("metadata change lost: %+v -> %+v: %v %v", pair[0], pair[1], diff, err)
		}
	}
}

func TestSnapshotSubtreeBoundariesAndCancellation(t *testing.T) {
	entries := []proto.ManifestEntry{file("a-b", 1), file("a/b", 1), file("a/c", 1), file("a0", 1)}
	s := mustNew(t, entries)
	for range 2 {
		var paths []string
		if err := s.Subtree(t.Context(), "a", func(e proto.ManifestEntry) bool { paths = append(paths, e.Path); return true }); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(paths, []string{"a/b", "a/c"}) {
			t.Fatal(paths)
		}
		ctx, cancel := context.WithCancel(t.Context())
		visits := 0
		err := s.Walk(ctx, func(proto.ManifestEntry) bool { visits++; cancel(); return true })
		if !errors.Is(err, context.Canceled) || visits != 1 {
			t.Fatalf("visits=%d err=%v", visits, err)
		}
		if _, err := s.tree(t.Context()); err != nil {
			t.Fatal(err)
		}
		s, err = s.Update(t.Context(), []Edit{{Entry: file("a/c", 2)}})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func file(p string, n int64) proto.ManifestEntry {
	return proto.ManifestEntry{Path: p, Type: proto.EntryFile, Mode: 0644, Size: n, SHA256: fmt.Sprintf("%064x", n)}
}
func mustNew(t *testing.T, entries []proto.ManifestEntry) *Snapshot {
	t.Helper()
	s, e := New(t.Context(), proto.Manifest{Entries: entries})
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func export(t *testing.T, s *Snapshot) proto.Manifest {
	t.Helper()
	m, e := s.Manifest(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	return m
}

func TestSnapshotOwnershipAndWireIdentity(t *testing.T) {
	for _, entries := range [][]proto.ManifestEntry{nil, {}, {file("a", 1), file("b", 2)}} {
		want := proto.Manifest{Entries: slices.Clone(entries)}
		s := mustNew(t, entries)
		if len(entries) > 0 {
			entries[0].Path = "changed"
			m := export(t, s)
			m.Entries[0].Size = 9
		}
		got, err := s.RootHash(t.Context())
		if err != nil || got != want.RootHash() {
			t.Fatalf("root %s: %v", got, err)
		}
		if !reflect.DeepEqual(export(t, s), want) {
			t.Fatal("export aliases caller or loses empty encoding")
		}
	}
}

func TestSnapshotUpdateEmptyIdentity(t *testing.T) {
	for _, entries := range [][]proto.ManifestEntry{nil, {}} {
		s := mustNew(t, entries)
		next, err := s.Update(t.Context(), []Edit{{Entry: proto.ManifestEntry{Path: "absent"}, Delete: true}})
		if err != nil {
			t.Fatal(err)
		}
		root, err := next.RootHash(t.Context())
		if err != nil || root != (proto.Manifest{Entries: entries}).RootHash() {
			t.Fatalf("no-op changed empty identity: %v", err)
		}
	}
	s := mustNew(t, []proto.ManifestEntry{file("a", 1)})
	if _, err := s.tree(t.Context()); err != nil {
		t.Fatal(err)
	}
	next, err := s.Update(t.Context(), []Edit{{Entry: file("a", 1), Delete: true}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := next.RootHash(t.Context())
	if err != nil || root != (proto.Manifest{}).RootHash() {
		t.Fatalf("deletion did not produce canonical empty identity: %v", err)
	}
}

func TestSnapshotRejectsMalformedStateAndUpdates(t *testing.T) {
	dir := proto.ManifestEntry{Path: "a", Type: proto.EntryDir, Mode: 0755}
	for _, entries := range [][]proto.ManifestEntry{{file("../x", 1)}, {file("a", 1), file("a", 2)}, {file("b", 1), file("a", 2)}, {file("a", 1), file("a/b", 1)}, {file("a", math.MaxInt64), file("b", 1)}} {
		if _, err := New(t.Context(), proto.Manifest{Entries: entries}); err == nil {
			t.Fatalf("accepted %v", entries)
		}
	}
	base := mustNew(t, []proto.ManifestEntry{dir, file("a/b", 1)})
	for _, edits := range [][]Edit{{{Entry: file("a", 1)}}, {{Entry: file("a/b", -1)}}, {{Entry: file("../x", 1), Delete: true}}, {{Entry: file("a/b", 2)}, {Entry: file("a/b", 3)}}, {{Entry: proto.ManifestEntry{Path: "link", Type: proto.EntrySymlink, Target: "../escape"}}}} {
		if _, err := base.Update(t.Context(), edits); err == nil {
			t.Fatalf("accepted edits %v", edits)
		}
	}
	// Validation uses the final hierarchy, allowing a directory replacement when
	// its descendants are explicitly removed in the same batch.
	next, err := base.Update(t.Context(), []Edit{{Entry: file("a", 2)}, {Entry: file("a/b", 0), Delete: true}})
	if err != nil {
		t.Fatal(err)
	}
	if next.Bytes() != 2 || base.Bytes() != 1 || next.Len() != 1 {
		t.Fatal("wrong totals")
	}
	big := mustNew(t, []proto.ManifestEntry{file("z", math.MaxInt64)})
	renamed, err := big.Update(t.Context(), []Edit{{Entry: file("a", math.MaxInt64)}, {Entry: file("z", 0), Delete: true}})
	if err != nil || renamed.Bytes() != math.MaxInt64 {
		t.Fatalf("transient batch overflow: %v", err)
	}
}

func TestSnapshotUpdatesMatchFullHierarchyValidation(t *testing.T) {
	current := mustNew(t, nil)
	rng := rand.New(rand.NewPCG(21, 4))
	paths := []string{"a", "a-b", "a/b", "a/b/c", "a/c", "a0", "b", "b/c"}
	for step := range 200 {
		before := export(t, current)
		wanted := map[string]proto.ManifestEntry{}
		for _, e := range before.Entries {
			wanted[e.Path] = e
		}
		var edits []Edit
		for _, i := range rng.Perm(len(paths))[:1+step%4] {
			e := file(paths[i], int64(step))
			switch rng.IntN(3) {
			case 0:
				e = proto.ManifestEntry{Path: paths[i], Type: proto.EntryDir}
			case 1:
				e = proto.ManifestEntry{Path: paths[i], Type: proto.EntrySymlink, Target: "target"}
			}
			remove := rng.IntN(3) == 0
			edits = append(edits, Edit{Entry: e, Delete: remove})
			if remove {
				delete(wanted, e.Path)
			} else {
				wanted[e.Path] = e
			}
		}
		var entries []proto.ManifestEntry
		for _, e := range wanted {
			entries = append(entries, e)
		}
		slices.SortFunc(entries, func(a, b proto.ManifestEntry) int { return strings.Compare(a.Path, b.Path) })
		fresh, fullErr := New(t.Context(), proto.Manifest{Entries: entries})
		next, err := current.Update(t.Context(), edits)
		if (err == nil) != (fullErr == nil) {
			t.Fatalf("validation differs for %v: %v / %v", edits, err, fullErr)
		}
		if !reflect.DeepEqual(before, export(t, current)) {
			t.Fatal("update mutated its input")
		}
		if err != nil {
			continue
		}
		root, _ := next.RootHash(t.Context())
		expected, _ := fresh.RootHash(t.Context())
		if root != expected || next.Bytes() != fresh.Bytes() {
			t.Fatalf("wrong state after %v", edits)
		}
		current = next
	}
}

func TestSnapshotDifferentialUpdates(t *testing.T) {
	entries := make([]proto.ManifestEntry, 1024)
	want := map[string]proto.ManifestEntry{}
	for i := range entries {
		entries[i] = file(fmt.Sprintf("file-%04d", i), int64(i))
		want[entries[i].Path] = entries[i]
	}
	current := mustNew(t, entries)
	if _, err := current.tree(t.Context()); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(8, 31))
	for step := range 100 {
		previous := current
		before := export(t, previous)
		var edits []Edit
		for _, i := range rng.Perm(1200)[:1+step%10] {
			e := file(fmt.Sprintf("file-%04d", i), int64(step))
			remove := rng.IntN(4) == 0
			edits = append(edits, Edit{Entry: e, Delete: remove})
			if remove {
				delete(want, e.Path)
			} else {
				want[e.Path] = e
			}
		}
		var err error
		current, err = current.Update(t.Context(), edits)
		if err != nil {
			t.Fatal(err)
		}
		expected := make([]proto.ManifestEntry, 0, len(want))
		var total int64
		for _, e := range want {
			expected = append(expected, e)
			total += e.Size
		}
		sort.Slice(expected, func(i, j int) bool { return expected[i].Path < expected[j].Path })
		got := export(t, current)
		if !slices.Equal(got.Entries, expected) || current.Bytes() != total {
			t.Fatal("wrong updated state")
		}
		if !reflect.DeepEqual(export(t, previous), before) {
			t.Fatal("mutated previous snapshot")
		}
		fresh := mustNew(t, expected)
		delta, err := previous.Diff(t.Context(), current)
		if err != nil {
			t.Fatal(err)
		}
		reference, err := mustNew(t, before.Entries).Diff(t.Context(), fresh)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(delta, reference) {
			t.Fatal("retained and cold diffs differ")
		}
		root, err := current.RootHash(t.Context())
		if err != nil || root != got.RootHash() {
			t.Fatalf("wrong wire hash: %v", err)
		}
	}
}

func TestSnapshotCancellationAndConcurrentCaches(t *testing.T) {
	entries := make([]proto.ManifestEntry, minIndexedEntries)
	for i := range entries {
		entries[i] = file(fmt.Sprintf("p-%04d", i), int64(i))
	}
	s := mustNew(t, entries)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := New(ctx, proto.Manifest{Entries: entries}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, []Edit{{Entry: file("p-0000", 2)}}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.Diff(ctx, s); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.Manifest(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.RootHash(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			next, err := s.Update(t.Context(), []Edit{{Entry: file("p-0000", int64(i))}})
			if err != nil {
				t.Error(err)
				return
			}
			_, err = next.RootHash(t.Context())
			if err != nil {
				t.Error(err)
			}
			_, err = s.RootHash(t.Context())
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if e, _ := s.Lookup("p-0000"); e.Size != 0 {
		t.Fatal("concurrent cache initialization changed source")
	}
}

func TestCancelledIndexBuildDoesNotPoisonSnapshot(t *testing.T) {
	entries := make([]proto.ManifestEntry, minIndexedEntries)
	for i := range entries {
		entries[i] = file(fmt.Sprintf("file-%04d", i), 1)
	}
	s := mustNew(t, entries)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	during := &cancelDuringWork{Context: ctx, cancel: cancel}
	during.remaining.Store(100)
	edit := []Edit{{Entry: file("file-0000", 2)}}
	if next, err := s.Update(during, edit); next != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("next=%v err=%v", next, err)
	}
	next, err := s.Update(t.Context(), edit)
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(entries)
	want[0] = edit[0].Entry
	if !slices.Equal(export(t, s).Entries, entries) || !slices.Equal(export(t, next).Entries, want) {
		t.Fatal("cancelled build changed subsequent results")
	}
}

func TestIndependentIndexesPreserveStructuralAndMetadataDiffs(t *testing.T) {
	base := []proto.ManifestEntry{{Path: "dir", Type: proto.EntryDir, Mode: 0755}, file("dir/child", 1), {Path: "link", Type: proto.EntrySymlink, Target: "dir/child"}}
	current := []proto.ManifestEntry{file("dir", 3), {Path: "link", Type: proto.EntrySymlink, Target: "dir"}}
	a, b := mustNew(t, base), mustNew(t, current)
	want, err := a.Diff(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	// Populate each index independently; no metadata change and no shared nodes.
	if _, err = a.tree(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = b.tree(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := a.Diff(t.Context(), b)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("diff=%v want=%v err=%v", got, want, err)
	}
	result, err := a.Update(t.Context(), got)
	if err != nil || !slices.Equal(export(t, result).Entries, current) {
		t.Fatalf("reconstruction: %v", err)
	}
}

func TestSnapshotAdversarialHeightFallsBack(t *testing.T) {
	// A monotone subsequence of path hashes creates a valid, deliberately skewed
	// Cartesian tree. Exercise public updates, including updates after fallback.
	const candidates = 12000
	entries := make([]proto.ManifestEntry, candidates)
	hashes := make([][32]byte, candidates)
	previous := make([]int, candidates)
	var tails []int
	for i := range entries {
		entries[i] = file(fmt.Sprintf("chosen-%05d", i), 1)
		hashes[i] = sha256.Sum256([]byte(entries[i].Path))
		j := sort.Search(len(tails), func(j int) bool { return bytes.Compare(hashes[tails[j]][:], hashes[i][:]) >= 0 })
		previous[i] = -1
		if j > 0 {
			previous[i] = tails[j-1]
		}
		if j == len(tails) {
			tails = append(tails, i)
		} else {
			tails[j] = i
		}
	}
	if len(tails) <= maxHeight {
		t.Fatal("fixture did not exceed height bound")
	}
	var chosen []proto.ManifestEntry
	for i := tails[len(tails)-1]; i >= 0; i = previous[i] {
		chosen = append(chosen, entries[i])
	}
	slices.Reverse(chosen)
	for len(chosen) < minIndexedEntries {
		chosen = append(chosen, file(fmt.Sprintf("z-%05d", len(chosen)), 1))
	}
	s := mustNew(t, chosen)
	if err := s.PrepareUpdates(t.Context()); err != nil {
		t.Fatal(err)
	}
	for n := int64(2); n <= 3; n++ {
		changed := chosen[0]
		changed.Size = n
		next, err := s.Update(t.Context(), []Edit{{Entry: changed}})
		if err != nil {
			t.Fatal(err)
		}
		chosen[0] = changed
		if !slices.Equal(export(t, next).Entries, chosen) || next.Bytes() != int64(len(chosen))-1+n {
			t.Fatal("incorrect fallback state")
		}
		if !next.noIndex.Load() {
			t.Fatal("unbounded index retained")
		}
		s = next
	}
}

func TestSnapshotUpdateRepresentationsAgree(t *testing.T) {
	tree, flat := mustNew(t, nil), mustNew(t, nil)
	flat.noIndex.Store(true)
	rng := rand.New(rand.NewPCG(21, 4))
	paths := []string{"a", "a-b", "a/b", "a/b/c", "a/c", "a0", "b", "b/c"}
	for step := range 200 {
		var edits []Edit
		for _, i := range rng.Perm(len(paths))[:1+step%4] {
			e := file(paths[i], int64(step))
			if rng.IntN(3) == 0 {
				e = proto.ManifestEntry{Path: paths[i], Type: proto.EntryDir}
			}
			edits = append(edits, Edit{Entry: e, Delete: rng.IntN(3) == 0})
		}
		if _, err := tree.tree(t.Context()); err != nil {
			t.Fatal(err)
		}
		a, ae := tree.Update(t.Context(), edits)
		b, be := flat.Update(t.Context(), edits)
		if (ae == nil) != (be == nil) {
			t.Fatalf("acceptance differs for %v: %v / %v", edits, ae, be)
		}
		if ae != nil {
			continue
		}
		ar, _ := a.RootHash(t.Context())
		br, _ := b.RootHash(t.Context())
		if !reflect.DeepEqual(export(t, a), export(t, b)) || a.Bytes() != b.Bytes() || ar != br {
			t.Fatalf("representations differ after %v", edits)
		}
		fresh := mustNew(t, export(t, b).Entries)
		if _, err := fresh.tree(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := a.tree(t.Context()); err != nil {
			t.Fatal(err)
		}
		if diff, err := a.Diff(t.Context(), fresh); err != nil || len(diff) != 0 {
			t.Fatalf("index and materialized view disagree: %v %v", diff, err)
		}
		tree, flat = a, b
	}
}
