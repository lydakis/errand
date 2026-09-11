package namedcache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestTreeHoldersSurviveRestartAndProtectEachOther(t *testing.T) {
	root := t.TempDir()
	s := openTestStore(t, root, 0)
	key := Key{"owner", "project", "arbitrary-name"}
	a, b := proto.NewULID(), proto.NewULID()
	data, err := s.AcquireTree(t.Context(), key, a)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := s.AcquireTree(t.Context(), key, b); err != nil || other != data {
		t.Fatalf("independent holder: %s %v", other, err)
	}
	if _, err := s.AcquireTree(t.Context(), key, a); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "object"), []byte("cached"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(t.Context(), key, proto.NewULID()); !errors.Is(err, ErrBusy) {
		t.Fatalf("exclusive access during shared use: %v", err)
	}
	if err := s.Discard(t.Context(), key, a); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("shared holder discarded store: %v", err)
	}
	if paths, err := s.LeasePaths(t.Context(), a, []Key{key}); err != nil || len(paths) != 0 {
		t.Fatalf("shared path used as process ownership: %v %v", paths, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, root, 0)
	entries, err := s.Inventory(t.Context())
	if err != nil || len(entries) != 1 || !slices.Contains(entries[0].Holders, a) || !slices.Contains(entries[0].Holders, b) || len(entries[0].Holders) != 2 {
		t.Fatalf("durable holders: %+v %v", entries, err)
	}
	if err := s.ReleaseTree(t.Context(), key, a); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseTree(t.Context(), key, a); err != nil {
		t.Fatal(err)
	}
	if gc, err := s.GC(t.Context(), false); err != nil || gc.Removed != 0 || gc.Protected != 1 {
		t.Fatalf("GC during sibling: %+v %v", gc, err)
	}
	if got, err := os.ReadFile(filepath.Join(data, "object")); err != nil || string(got) != "cached" {
		t.Fatalf("sibling storage changed: %q %v", got, err)
	}
	if err := s.ReleaseTree(t.Context(), key, b); err != nil {
		t.Fatal(err)
	}
	if gc, err := s.GC(t.Context(), false); err != nil || gc.Removed != 1 || gc.FreedBytes != 6 {
		t.Fatalf("idle GC: %+v %v", gc, err)
	}
}

func TestTreeHoldersDoNotImposeMetadataConcurrencyLimit(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1<<20)
	// Even large escaped identities leave holder count independent of the
	// record size limit. The previous inline-list design stopped below 400.
	key := Key{strings.Repeat("\t", 512), strings.Repeat("\t", 512), "cache"}
	acquired := 0
	for range 410 {
		_, err := s.AcquireTree(t.Context(), key, proto.NewULID())
		if err != nil {
			t.Fatal(err)
		}
		acquired++
	}
	entries, err := s.Inventory(t.Context())
	if acquired == 0 || err != nil || len(entries) != 1 || len(entries[0].Holders) != acquired {
		t.Fatalf("metadata limit damaged holders: %d %+v %v", acquired, entries, err)
	}
}

func TestFailedTreeReleasePreservesEveryHolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses fixture write permissions")
	}
	s := openTestStore(t, t.TempDir(), 1<<20)
	key, a, b := Key{"owner", "project", "cache"}, proto.NewULID(), proto.NewULID()
	data, err := s.AcquireTree(t.Context(), key, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireTree(t.Context(), key, b); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Dir(data)
	if err := os.Chmod(entry, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(entry, 0700) })
	if err := s.ReleaseTree(t.Context(), key, a); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("expected failed durable release: %v", err)
	}
	entries, err := s.Inventory(t.Context())
	want := []string{a, b}
	slices.Sort(want)
	if err != nil || len(entries) != 1 || !slices.Equal(entries[0].Holders, want) {
		t.Fatalf("lost holder on release failure: %+v %v", entries, err)
	}
	if gc, err := s.GC(t.Context(), false); err != nil || gc.Protected != 1 || gc.Removed != 0 {
		t.Fatalf("failed release permitted eviction: %+v %v", gc, err)
	}
}

func TestTreeHolderAdoptsOnlyIdleLegacyCacheAndKeepsScope(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1<<20)
	key, a, b := Key{"owner", "project", "store"}, proto.NewULID(), proto.NewULID()
	data, err := s.Acquire(t.Context(), key, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireTree(t.Context(), key, b); !errors.Is(err, ErrBusy) {
		t.Fatalf("adopted busy legacy entry: %v", err)
	}
	if err := s.Release(t.Context(), key, a); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AcquireTree(t.Context(), key, b); err != nil || got != data {
		t.Fatalf("lost warm storage: %s %v", got, err)
	}
	for _, separate := range []Key{{"other", key.Project, key.Name}, {key.Owner, "other", key.Name}, {key.Owner, key.Project, "other"}} {
		if got, err := s.AcquireTree(t.Context(), separate, proto.NewULID()); err != nil || got == data {
			t.Fatalf("scope: %s %v", got, err)
		}
	}
	if err := s.ReleaseTree(t.Context(), key, b); err != nil {
		t.Fatal(err)
	}
	// The legacy recovery API remains available once shared holders settle.
	if got, err := s.Acquire(t.Context(), key, a); err != nil || got != data {
		t.Fatalf("legacy fallback: %s %v", got, err)
	}
}

func TestTreeHolderUnreadableStoreDoesNotBlockUnrelatedGC(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires unprivileged permissions")
	}
	s := openTestStore(t, t.TempDir(), 0)
	var bad string
	for _, name := range []string{"unreadable", "healthy"} {
		key, id := Key{"owner", "project", name}, proto.NewULID()
		data, err := s.AcquireTree(t.Context(), key, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(data, "object"), []byte("cached"), 0600); err != nil {
			t.Fatal(err)
		}
		if name == "unreadable" {
			bad = data
			if err := os.Chmod(data, 0000); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.ReleaseTree(t.Context(), key, id); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0700) })
	gc, err := s.GC(t.Context(), false)
	if err != nil || gc.Removed < 1 {
		t.Fatalf("an idle unreadable store blocked collection of unrelated garbage: result=%+v error=%v", gc, err)
	}
}

func TestTreeHolderIdleMissingRootCanBeReused(t *testing.T) {
	for _, collect := range []bool{false, true} {
		t.Run(fmt.Sprint(collect), func(t *testing.T) {
			s := openTestStore(t, t.TempDir(), 1<<20)
			key, id := Key{"owner", "project", "cache"}, proto.NewULID()
			data, err := s.AcquireTree(t.Context(), key, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(data); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AcquireTree(t.Context(), key, proto.NewULID()); !errors.Is(err, ErrBusy) {
				t.Fatalf("repaired a root with unresolved holder: %v", err)
			}
			if err := s.ReleaseTree(t.Context(), key, id); err != nil {
				t.Fatal(err)
			}
			if collect {
				if _, err := s.GC(t.Context(), false); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.AcquireTree(t.Context(), key, proto.NewULID()); err != nil {
				t.Fatalf("idle missing root cannot be reused: %v", err)
			}
		})
	}
}

func TestTreeHolderGCDoesNotFollowIdleReplacement(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1<<20)
	key, id := Key{"owner", "project", "cache"}, proto.NewULID()
	data, err := s.AcquireTree(t.Context(), key, id)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(data); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, data); err != nil {
		t.Fatal(err)
	}
	if gc, err := s.GC(t.Context(), false); err != nil || gc.Protected != 1 {
		t.Fatalf("live replacement lost protection: %+v %v", gc, err)
	}
	if err := s.ReleaseTree(t.Context(), key, id); err != nil {
		t.Fatal(err)
	}
	if gc, err := s.GC(t.Context(), false); err != nil || gc.Removed != 1 {
		t.Fatalf("idle replacement not retired: %+v %v", gc, err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "keep" {
		t.Fatalf("GC followed external replacement: %q %v", got, err)
	}
}

func TestTreeHolderSizeIsUnknownUntilIdleMeasurement(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1<<20)
	key, id := Key{"owner", "project", "cache"}, proto.NewULID()
	data, err := s.AcquireTree(t.Context(), key, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "object"), []byte("cached"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseTree(t.Context(), key, id); err != nil {
		t.Fatal(err)
	}
	entries, err := s.Inventory(t.Context())
	if err != nil || len(entries) != 1 || !entries[0].BytesUnknown {
		t.Fatalf("unmeasured cache looks empty: %+v %v", entries, err)
	}
	if _, err := s.GC(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	entries, err = s.Inventory(t.Context())
	if err != nil || len(entries) != 1 || entries[0].BytesUnknown || entries[0].Bytes != 6 {
		t.Fatalf("size not measured: %+v %v", entries, err)
	}
}
