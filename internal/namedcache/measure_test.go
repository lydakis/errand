package namedcache

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

func useTree(t *testing.T, s *Store, key Key, content string) {
	t.Helper()
	id := proto.NewULID()
	data, err := s.AcquireTree(t.Context(), key, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "object"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseTree(t.Context(), key, id); err != nil {
		t.Fatal(err)
	}
}

func entryFor(t *testing.T, s *Store, key Key) Entry {
	t.Helper()
	entries, err := s.Inventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Key == key {
			return entry
		}
	}
	t.Fatalf("no named cache %+v in %+v", key, entries)
	return Entry{}
}

func everyEntry(Entry) bool { return true }

func TestMeasureUnknownRecordsWhatGCWouldFree(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 0)
	key := Key{"owner", "project", "cache"}
	useTree(t, s, key, "cached")
	if entry := entryFor(t, s, key); !entry.BytesUnknown {
		t.Fatalf("unused tree cache already measured: %+v", entry)
	}
	if err := s.MeasureUnknown(t.Context(), everyEntry, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	entry := entryFor(t, s, key)
	if entry.BytesUnknown || entry.Bytes != int64(len("cached")) {
		t.Fatalf("idle cache not measured: %+v", entry)
	}
	plan, err := s.GC(t.Context(), true)
	if err != nil || plan.Removed != 1 || plan.FreedBytes != entry.Bytes {
		t.Fatalf("inventory %d bytes, GC plan %+v %v", entry.Bytes, plan, err)
	}
}

func TestReleaseMarksTreeSizeStaleForRemeasurement(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1<<20)
	key := Key{"owner", "project", "cache"}
	useTree(t, s, key, "v1")
	if err := s.MeasureUnknown(t.Context(), everyEntry, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	useTree(t, s, key, "version two")
	if entry := entryFor(t, s, key); !entry.BytesUnknown || entry.Bytes != 2 {
		t.Fatalf("released tree kept a current-looking size: %+v", entry)
	}
	if err := s.MeasureUnknown(t.Context(), everyEntry, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if entry := entryFor(t, s, key); entry.BytesUnknown || entry.Bytes != int64(len("version two")) {
		t.Fatalf("stale size not remeasured: %+v", entry)
	}
}

func TestMeasureUnknownSkipsHeldExcludedAndLateEntries(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1<<20)
	held := Key{"owner", "project", "held"}
	other := Key{"other", "project", "cache"}
	late := Key{"owner", "project", "late"}
	useTree(t, s, other, "theirs")
	useTree(t, s, late, "later")
	if _, err := s.AcquireTree(t.Context(), held, proto.NewULID()); err != nil {
		t.Fatal(err)
	}
	owned := func(entry Entry) bool { return entry.Key.Owner == "owner" }
	if err := s.MeasureUnknown(t.Context(), owned, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if entry := entryFor(t, s, late); !entry.BytesUnknown {
		t.Fatalf("measurement started after its deadline: %+v", entry)
	}
	if err := s.MeasureUnknown(t.Context(), owned, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if entry := entryFor(t, s, late); entry.BytesUnknown || entry.Bytes != int64(len("later")) {
		t.Fatalf("idle owned cache not measured: %+v", entry)
	}
	if entry := entryFor(t, s, held); !entry.BytesUnknown {
		t.Fatalf("held cache measured while in use: %+v", entry)
	}
	if entry := entryFor(t, s, other); !entry.BytesUnknown {
		t.Fatalf("excluded owner's cache measured: %+v", entry)
	}
}

func TestMeasurementIsDroppedWhenCacheIsUsedMeanwhile(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1<<20)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	key := Key{"owner", "project", "cache"}
	useTree(t, s, key, "before")
	sample := entryFor(t, s, key)
	now = now.Add(time.Minute)
	useTree(t, s, key, "after the sample")
	if err := s.recordMeasurement(t.Context(), sample, 1); err != nil {
		t.Fatal(err)
	}
	if entry := entryFor(t, s, key); !entry.BytesUnknown || entry.Bytes == 1 {
		t.Fatalf("stale measurement published: %+v", entry)
	}
}

func TestMeasureUnknownExcludesOrphanGenerationsAsGCDoes(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1<<20)
	key := Key{"owner", "project", "cache"}
	useTree(t, s, key, "cached")
	orphan := filepath.Join(s.dir, key.hash(), "trees", "generation-"+proto.NewULID(), "tree")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "payload"), make([]byte, 64), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.MeasureUnknown(t.Context(), everyEntry, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	entry := entryFor(t, s, key)
	if entry.BytesUnknown || entry.Bytes != int64(len("cached")) {
		t.Fatalf("orphaned generation counted as cache data: %+v", entry)
	}
	s.maxBytes = 0
	plan, err := s.GC(t.Context(), true)
	if err != nil || plan.Removed != 1 || plan.FreedBytes != entry.Bytes || plan.ReclaimedTemps != 1 {
		t.Fatalf("inventory %d bytes, GC plan %+v %v", entry.Bytes, plan, err)
	}
}

func TestMeasureUnknownResumesAfterCacheThatFailed(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1<<20)
	keys := []Key{{"owner", "project", "a"}, {"owner", "project", "b"}}
	for _, key := range keys {
		useTree(t, s, key, "cached")
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].hash() < keys[j].hash() })
	broken, healthy := keys[0], keys[1]
	data := filepath.Join(s.dir, broken.hash(), "data")
	if err := os.RemoveAll(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(data, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Every clock read is a minute later, so each call starts one measurement,
	// as when a large cache takes the whole budget before failing.
	now := time.Now()
	s.now = func() time.Time { now = now.Add(time.Minute); return now }
	for range 2 {
		if err := s.MeasureUnknown(t.Context(), everyEntry, now.Add(90*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if entry := entryFor(t, s, broken); !entry.BytesUnknown {
		t.Fatalf("broken cache measured: %+v", entry)
	}
	if entry := entryFor(t, s, healthy); entry.BytesUnknown || entry.Bytes != int64(len("cached")) {
		t.Fatalf("failing cache kept every call from reaching the next: %+v", entry)
	}
}

func TestMeasureUnknownRemeasuresSizesRecordedBeforeUpgrade(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1<<20)
	key := Key{"owner", "project", "cache"}
	useTree(t, s, key, "v1")
	// Releases used to keep the size GC recorded, although the job changed the tree.
	if err := s.lock(t.Context()); err != nil {
		t.Fatal(err)
	}
	r, err := s.read(key.hash())
	if err == nil {
		r.Bytes, r.BytesUnknown = 1, false
		err = s.write(key.hash(), r)
	}
	s.unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MeasureUnknown(t.Context(), everyEntry, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if entry := entryFor(t, s, key); entry.BytesUnknown || entry.Bytes != int64(len("v1")) {
		t.Fatalf("size recorded before upgrade was trusted: %+v", entry)
	}
	if _, err := os.Stat(filepath.Join(s.dir, legacySizesMarker)); err != nil {
		t.Fatalf("upgrade invalidation not recorded: %v", err)
	}
}
