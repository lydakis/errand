//go:build darwin || linux

package snapshotcheckpoint

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/snapshot"
)

func derivedOracle(t *testing.T, root, cache string, journal bool) Result {
	t.Helper()
	got, err := PrepareDerived(t.Context(), root, cache, snapshot.SelectOptions{}, journal)
	if err != nil || got.CacheError != "" {
		t.Fatalf("derived: %+v %v", got, err)
	}
	want, err := Cold(t.Context(), root, snapshot.SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := got.State.RootHash(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	b, err := want.State.RootHash(t.Context())
	if err != nil || a != b {
		t.Fatalf("roots differ %s %s %v", a, b, err)
	}
	return got
}

func TestDerivedRestartJournalCompactionAndRecovery(t *testing.T) {
	for _, journal := range []bool{false, true} {
		t.Run(fmt.Sprint(journal), func(t *testing.T) {
			root, cache := fixture(t)
			seed := derivedOracle(t, root, cache, journal)
			if !seed.Written || seed.Hashed != 3 {
				t.Fatalf("seed %+v", seed)
			}
			warm := derivedOracle(t, root, cache, journal)
			if warm.Written || warm.Reused != 3 {
				t.Fatalf("warm %+v", warm)
			}
			for i := 0; i < maxJournalRecords+1; i++ {
				if err := os.WriteFile(filepath.Join(root, "a"), []byte(fmt.Sprint(i)), 0600); err != nil {
					t.Fatal(err)
				}
				got := derivedOracle(t, root, cache, journal)
				if !got.Written || got.Hashed != 1 || got.Reused != 2 {
					t.Fatalf("edit %+v", got)
				}
				loadedRecords := 0
				if journal {
					loadedRecords = i
				}
				if got.JournalRecordsLoaded != loadedRecords || got.ReplacedBase != (!journal || i == maxJournalRecords) {
					t.Fatalf("publication receipt: %+v", got)
				}
			}
			if err := os.Remove(filepath.Join(root, "a")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../b", filepath.Join(root, "nested", "link")); err != nil {
				t.Fatal(err)
			}
			derivedOracle(t, root, cache, journal)
			// A torn trailing transaction cannot hide the last verified generation.
			f, err := os.OpenFile(filepath.Join(cache, "index"), os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				t.Fatal(err)
			}
			f.Write([]byte("partial frame"))
			f.Close()
			got := derivedOracle(t, root, cache, journal)
			if got.CacheStatus != "recovered" || !got.ReplacedBase || !got.Written {
				t.Fatalf("recovery %+v", got)
			}
			got = derivedOracle(t, root, cache, journal)
			if got.CacheStatus != "hit" || got.Written {
				t.Fatalf("recovered warm %+v", got)
			}
		})
	}
}

func TestDerivedLargeTreeAndStaleWriter(t *testing.T) {
	root, cache := fixture(t)
	root, _ = filepath.EvalSymlinks(root)
	for i := 0; i < 4200; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%05d", i)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	derivedOracle(t, root, cache, true)
	if got := derivedOracle(t, root, cache, true); got.CacheStatus != "hit" || got.Hashed != 0 {
		t.Fatalf("large restart: %+v", got)
	}
	_, _, policy, _, err := snapshot.SelectFilesGuarded(root, snapshot.SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	key, err := checkoutIdentity(root, policy, snapshot.SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	old, status, _, err := readDerived(t.Context(), cache, key)
	if err != nil || status != "hit" {
		t.Fatalf("load %s %v", status, err)
	}
	if err := os.WriteFile(filepath.Join(root, "f00000"), []byte("y"), 0600); err != nil {
		t.Fatal(err)
	}
	got := derivedOracle(t, root, cache, true)
	if got.Hashed != 1 || got.CheckpointBytes >= 10000 {
		t.Fatalf("not a small journal append: %+v", got)
	}
	verified := verifiedFixture(t, root)
	before, _ := os.ReadFile(filepath.Join(cache, "index"))
	if _, _, err := writeDerived(t.Context(), cache, key, verified, got.State, old, true); err == nil {
		t.Fatal("stale writer appended")
	}
	after, _ := os.ReadFile(filepath.Join(cache, "index"))
	if string(before) != string(after) {
		t.Fatal("stale writer changed checkpoint")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := PrepareDerived(ctx, root, cache, snapshot.SelectOptions{}, true); err != context.Canceled {
		t.Fatal(err)
	}
	derivedOracle(t, root, cache, true)
}

func TestDerivedRejectsInvalidPublicationAndCorruptBase(t *testing.T) {
	root, cache := fixture(t)
	got := derivedOracle(t, root, cache, true)
	if _, _, err := writeDerived(t.Context(), cache, identity{Root: root}, snapshot.VerifiedObservations{}, got.State, loadedCheckpoint{}, false); err == nil {
		t.Fatal("accepted invalid batch")
	}
	file := filepath.Join(cache, "index")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 1
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	got = derivedOracle(t, root, cache, true)
	if got.CacheStatus != "corrupt" || got.Reused != 0 {
		t.Fatalf("corruption reused: %+v", got)
	}
}

func TestDerivedBusyCacheStillReturnsFreshSnapshot(t *testing.T) {
	root, cache := fixture(t)
	derivedOracle(t, root, cache, true)
	lock, err := derivedLock(cache, true)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockDerived(lock)
	got, err := PrepareDerived(t.Context(), root, cache, snapshot.SelectOptions{}, true)
	if err != nil || got.State == nil || got.Reused != 0 || got.Hashed != 3 || got.Written || got.CacheError == "" {
		t.Fatalf("busy cache blocked fresh result: %+v %v", got, err)
	}
}

func TestJournalCoalescesObservationEditsAcrossRestarts(t *testing.T) {
	root, cache := fixture(t)
	derivedOracle(t, root, cache, true)
	// Both a preexisting entry and a journal-only entry are deleted, recreated
	// with different metadata, and deleted again before any compaction.
	for _, name := range []string{"a", "new"} {
		file := filepath.Join(root, name)
		for step := 0; step < 6; step++ {
			var err error
			if step%2 == 0 {
				err = os.WriteFile(file, []byte(fmt.Sprint(name, step)), 0600)
			} else {
				err = os.Remove(file)
			}
			if err != nil {
				t.Fatal(err)
			}
			got := derivedOracle(t, root, cache, true)
			if got.CacheStatus != "hit" || !got.Written || got.ReplacedBase {
				t.Fatalf("unexpected rebuild: %+v", got)
			}
		}
	}
	got := derivedOracle(t, root, cache, true)
	if got.Written || got.Hashed != 0 {
		t.Fatalf("replayed state was not reusable: %+v", got)
	}
}

func loadDerivedFixture(t *testing.T, root, cache string) (loadedCheckpoint, identity) {
	t.Helper()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	_, _, policy, _, err := snapshot.SelectFilesGuarded(root, snapshot.SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	key, err := checkoutIdentity(root, policy, snapshot.SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	loaded, status, _, err := readDerived(t.Context(), cache, key)
	if err != nil || status != "hit" {
		t.Fatalf("load: %s %v", status, err)
	}
	return loaded, key
}

func TestDerivedByteBudgetCompaction(t *testing.T) {
	for _, budget := range []string{"journal", "file"} {
		for _, excess := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/excess=%d", budget, excess), func(t *testing.T) {
				root, cache := fixture(t)
				derivedOracle(t, root, cache, true)
				prior, key := loadDerivedFixture(t, root, cache)
				if err := os.WriteFile(filepath.Join(root, "a"), []byte("updated"), 0600); err != nil {
					t.Fatal(err)
				}
				verified := verifiedFixture(t, key.Root)
				state, err := prior.state.Update(t.Context(), differences(prior.Entries, verified))
				if err != nil {
					t.Fatal(err)
				}
				payload, err := encodeDerived(t.Context(), key, verified, state, prior, true)
				if err != nil {
					t.Fatal(err)
				}
				// Exercise both writer admission boundaries without building 64 MiB
				// fixtures. Only byte accounting is synthetic; source observations,
				// generation checks, publication and restart validation are real.
				frameBytes := len(payload) + frameOverhead
				if budget == "journal" {
					prior.journalBytes = maxJournalBytes - frameBytes + excess
				} else {
					prior.diskSize = maxBytes - frameBytes + excess
				}
				written, replaced, err := writeDerived(t.Context(), cache, key, verified, state, prior, true)
				if err != nil || written == 0 || replaced != (excess > 0) {
					t.Fatalf("publication: bytes=%d replaced=%v err=%v", written, replaced, err)
				}
				loaded, _ := loadDerivedFixture(t, root, cache)
				wantRecords := 1 - excess
				if loaded.journalRecords != wantRecords {
					t.Fatalf("journal records=%d, want %d", loaded.journalRecords, wantRecords)
				}
				got := derivedOracle(t, root, cache, true)
				if got.Written || got.Hashed != 0 || got.JournalRecordsLoaded != wantRecords {
					t.Fatalf("published state not reusable: %+v", got)
				}
			})
		}
	}
}

func TestDerivedRecoveryDiscardsDamagedFrameAndFollowingTransactions(t *testing.T) {
	for _, damage := range []string{"checksum", "truncated-payload"} {
		t.Run(damage, func(t *testing.T) {
			root, cache := fixture(t)
			derivedOracle(t, root, cache, true)
			if err := os.WriteFile(filepath.Join(root, "a"), []byte("first"), 0600); err != nil {
				t.Fatal(err)
			}
			prefix := derivedOracle(t, root, cache, true)
			loaded, key := loadDerivedFixture(t, root, cache)
			badFrame := loaded.diskSize
			for _, edit := range []struct{ path, body string }{{"a", "second"}, {"b", "third"}} {
				if err := os.WriteFile(filepath.Join(root, edit.path), []byte(edit.body), 0600); err != nil {
					t.Fatal(err)
				}
				derivedOracle(t, root, cache, true)
			}
			file := filepath.Join(cache, "index")
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			payloadLength := int(binary.LittleEndian.Uint64(data[badFrame:]))
			middle := badFrame + 40 + payloadLength/2
			if damage == "checksum" {
				data[middle] ^= 1 // Leave a complete later transaction after the bad frame.
			} else {
				data = data[:middle] // Length and previous digest survive; the payload does not.
			}
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			recovered, status, _, err := readDerived(t.Context(), cache, key)
			if err != nil || status != "recovered" || recovered.journalRecords != 1 {
				t.Fatalf("recovery: status=%s records=%d err=%v", status, recovered.journalRecords, err)
			}
			if diff, err := prefix.State.Diff(t.Context(), recovered.state); err != nil || len(diff) != 0 {
				t.Fatalf("did not recover exact valid prefix: %v %v", diff, err)
			}
			got := derivedOracle(t, root, cache, true)
			if !got.Written || !got.ReplacedBase || got.JournalRecordsLoaded != 1 || got.Hashed != 2 || got.Reused != 1 {
				t.Fatalf("recovery did not refresh source and replace base: %+v", got)
			}
			got = derivedOracle(t, root, cache, true)
			if got.Written || got.Hashed != 0 || got.JournalRecordsLoaded != 0 {
				t.Fatalf("recovered base is not reusable: %+v", got)
			}
		})
	}
}
