//go:build darwin || linux

package snapshotcheckpoint

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/snapshot"
)

func TestObservationJournalFormatAndStampOnlyReplay(t *testing.T) {
	root, cache := fixture(t)
	s := observationJournalStore()
	for i := 0; i < 4200; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%05d", i)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	seed := framedOracle(t, root, cache, s)
	data, err := os.ReadFile(filepath.Join(cache, s.name))
	if err != nil {
		t.Fatal(err)
	}
	length := int(binary.LittleEndian.Uint64(data[len(s.magic):]))
	var header derivedHeader
	if err := gob.NewDecoder(bytes.NewReader(data[len(s.magic)+40 : len(s.magic)+40+length])).Decode(&header); err != nil {
		t.Fatal(err)
	}
	if header.IndexCount != 0 || header.IndexVersion != 0 || header.Fallback || header.Count != seed.State.Len() {
		t.Fatalf("stored derived state: %+v", header)
	}
	old, key := loadFramedFixture(t, root, cache, s)
	if got := framedOracle(t, root, cache, s); got.Hashed != 0 || got.Written {
		t.Fatalf("warm: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("new body"), 0600); err != nil {
		t.Fatal(err)
	}
	got := framedOracle(t, root, cache, s)
	if got.Hashed != 1 || got.CheckpointBytes >= 10000 || got.ReplacedBase {
		t.Fatalf("append: %+v", got)
	}
	verified := verifiedFixture(t, key.Root)
	// A stamp-only edit must survive the journal even though tree metadata is equal.
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("new body"), 0600); err != nil {
		t.Fatal(err)
	}
	got = framedOracle(t, root, cache, s)
	if got.Hashed != 1 || !got.Written {
		t.Fatalf("stamp update: %+v", got)
	}
	if got := framedOracle(t, root, cache, s); got.Hashed != 0 || got.Written || got.JournalRecordsLoaded != 2 {
		t.Fatalf("stamp replay: %+v", got)
	}
	// Derived images are never accepted by the observation-only decoder.
	payload, err := derivedStore(false).encode(t.Context(), key, verified, got.State, old, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.decode(t.Context(), payload, key, loadedCheckpoint{}, false); err == nil {
		t.Fatal("accepted a derived image")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := PrepareObservationJournal(ctx, root, cache, snapshot.SelectOptions{}); err != context.Canceled {
		t.Fatal(err)
	}
}
