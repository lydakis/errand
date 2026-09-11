package namedcache

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestLeaseLookupUsesOnlyReceiptKeys(t *testing.T) {
	s := openTestStore(t, t.TempDir(), 1<<20)
	key, id := Key{"owner", "project", "mine"}, proto.NewULID()
	data, err := s.Acquire(t.Context(), key, id)
	if err != nil {
		t.Fatal(err)
	}
	other := Key{"different-owner", "different-project", "broken"}
	otherData, err := s.AcquireTree(t.Context(), other, proto.NewULID())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(otherData), "record.json"), []byte("unreadable record"), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := s.LookupLease(t.Context(), key, id)
	if err != nil || !state.Directory || state.Tree {
		t.Fatalf("unrelated state blocked lease lookup: %+v %v", state, err)
	}
	canonicalData, err := filepath.EvalSymlinks(data)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := s.LeasePaths(t.Context(), id, []Key{key})
	if err != nil || len(paths) != 1 || paths[0] != canonicalData {
		t.Fatalf("unrelated state blocked process scope: %v %v", paths, err)
	}
	if err := s.Release(t.Context(), key, id); err != nil {
		t.Fatal(err)
	}
	state, err = s.LookupLease(t.Context(), key, id)
	if err != nil || state.Directory || state.Tree {
		t.Fatalf("released lease remained live: %+v %v", state, err)
	}
	// Missing metadata under a selected key is not an absent lease.
	if _, err := s.LookupLease(t.Context(), other, id); err == nil {
		t.Fatal("damaged selected record treated as absent")
	}
}
