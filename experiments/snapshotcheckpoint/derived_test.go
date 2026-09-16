//go:build darwin || linux

package snapshotcheckpoint

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/snapshot"
)

func framedOracle(t *testing.T, root, cache string, store framedStore) Result {
	t.Helper()
	config := defaultPreparation(false)
	config.store = store
	got, err := prepare(t.Context(), root, cache, snapshot.SelectOptions{}, config)
	if err != nil || got.CacheError != "" {
		t.Fatalf("framed checkpoint: %+v %v", got, err)
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

func TestFramedRestartJournalCompactionAndRecovery(t *testing.T) {
	replacement := observationJournalStore()
	replacement.journal = false
	for _, store := range []framedStore{derivedStore(false), derivedStore(true), replacement, observationJournalStore()} {
		t.Run(fmt.Sprintf("%s/journal=%v", store.name, store.journal), func(t *testing.T) {
			journal := store.journal
			root, cache := fixture(t)
			seed := framedOracle(t, root, cache, store)
			if !seed.Written || seed.Hashed != 3 {
				t.Fatalf("seed %+v", seed)
			}
			warm := framedOracle(t, root, cache, store)
			if warm.Written || warm.Reused != 3 {
				t.Fatalf("warm %+v", warm)
			}
			for i := 0; i < maxJournalRecords+1; i++ {
				if err := os.WriteFile(filepath.Join(root, "a"), []byte(fmt.Sprint(i)), 0600); err != nil {
					t.Fatal(err)
				}
				got := framedOracle(t, root, cache, store)
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
			framedOracle(t, root, cache, store)
			// A torn trailing transaction cannot hide the last verified generation.
			f, err := os.OpenFile(filepath.Join(cache, store.name), os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				t.Fatal(err)
			}
			f.Write([]byte("partial frame"))
			f.Close()
			got := framedOracle(t, root, cache, store)
			if got.CacheStatus != "recovered" || !got.ReplacedBase || !got.Written {
				t.Fatalf("recovery %+v", got)
			}
			got = framedOracle(t, root, cache, store)
			if got.CacheStatus != "hit" || got.Written {
				t.Fatalf("recovered warm %+v", got)
			}
		})
	}
}

func TestFramedLargeTreeAndStaleWriter(t *testing.T) {
	for _, store := range []framedStore{derivedStore(true), observationJournalStore()} {
		t.Run(store.name, func(t *testing.T) {
			root, cache := fixture(t)
			root, _ = filepath.EvalSymlinks(root)
			for i := 0; i < 4200; i++ {
				if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%05d", i)), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			framedOracle(t, root, cache, store)
			if got := framedOracle(t, root, cache, store); got.CacheStatus != "hit" || got.Hashed != 0 {
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
			old, status, _, err := store.load(t.Context(), cache, key)
			if err != nil || status != "hit" {
				t.Fatalf("load %s %v", status, err)
			}
			if err := os.WriteFile(filepath.Join(root, "f00000"), []byte("y"), 0600); err != nil {
				t.Fatal(err)
			}
			got := framedOracle(t, root, cache, store)
			if got.Hashed != 1 || got.CheckpointBytes >= 10000 {
				t.Fatalf("not a small journal append: %+v", got)
			}
			verified := verifiedFixture(t, root)
			before, _ := os.ReadFile(filepath.Join(cache, store.name))
			if _, _, err := store.save(t.Context(), cache, key, verified, got.State, old); err == nil {
				t.Fatal("stale writer appended")
			}
			after, _ := os.ReadFile(filepath.Join(cache, store.name))
			if string(before) != string(after) {
				t.Fatal("stale writer changed checkpoint")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			config := defaultPreparation(false)
			config.store = store
			if _, err := prepare(ctx, root, cache, snapshot.SelectOptions{}, config); err != context.Canceled {
				t.Fatal(err)
			}
			framedOracle(t, root, cache, store)
		})
	}
}

func TestFramedRejectsInvalidPublicationAndCorruptBase(t *testing.T) {
	for _, store := range []framedStore{derivedStore(true), observationJournalStore()} {
		t.Run(store.name, func(t *testing.T) {
			root, cache := fixture(t)
			got := framedOracle(t, root, cache, store)
			if _, _, err := store.save(t.Context(), cache, identity{Root: root}, snapshot.VerifiedObservations{}, got.State, loadedCheckpoint{}); err == nil {
				t.Fatal("accepted invalid batch")
			}
			file := filepath.Join(cache, store.name)
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			data[len(data)/2] ^= 1
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			got = framedOracle(t, root, cache, store)
			if got.CacheStatus != "corrupt" || got.Reused != 0 {
				t.Fatalf("corruption reused: %+v", got)
			}
		})
	}
}

func TestFramedBusyCacheStillReturnsFreshSnapshot(t *testing.T) {
	for _, store := range []framedStore{derivedStore(true), observationJournalStore()} {
		t.Run(store.name, func(t *testing.T) {
			root, cache := fixture(t)
			framedOracle(t, root, cache, store)
			lock, err := store.lock(cache, true)
			if err != nil {
				t.Fatal(err)
			}
			defer unlockDerived(lock)
			config := defaultPreparation(false)
			config.store = store
			got, err := prepare(t.Context(), root, cache, snapshot.SelectOptions{}, config)
			if err != nil || got.State == nil || got.Reused != 0 || got.Hashed != 3 || got.Written || got.CacheError == "" {
				t.Fatalf("busy cache blocked fresh result: %+v %v", got, err)
			}
		})
	}
}

func TestJournalCoalescesObservationEditsAcrossRestarts(t *testing.T) {
	for _, store := range []framedStore{derivedStore(true), observationJournalStore()} {
		t.Run(store.name, func(t *testing.T) {
			root, cache := fixture(t)
			framedOracle(t, root, cache, store)
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
					got := framedOracle(t, root, cache, store)
					if got.CacheStatus != "hit" || !got.Written || got.ReplacedBase {
						t.Fatalf("unexpected rebuild: %+v", got)
					}
				}
			}
			got := framedOracle(t, root, cache, store)
			if got.Written || got.Hashed != 0 {
				t.Fatalf("replayed state was not reusable: %+v", got)
			}
		})
	}
}

func loadFramedFixture(t *testing.T, root, cache string, store framedStore) (loadedCheckpoint, identity) {
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
	loaded, status, _, err := store.load(t.Context(), cache, key)
	if err != nil || status != "hit" {
		t.Fatalf("load: %s %v", status, err)
	}
	return loaded, key
}

func TestDerivedByteBudgetCompaction(t *testing.T) {
	for _, store := range []framedStore{derivedStore(true), observationJournalStore()} {
		t.Run(store.name, func(t *testing.T) {

			for _, budget := range []string{"journal", "file"} {
				for _, excess := range []int{0, 1} {
					t.Run(fmt.Sprintf("%s/excess=%d", budget, excess), func(t *testing.T) {
						root, cache := fixture(t)
						framedOracle(t, root, cache, store)
						prior, key := loadFramedFixture(t, root, cache, store)
						if err := os.WriteFile(filepath.Join(root, "a"), []byte("updated"), 0600); err != nil {
							t.Fatal(err)
						}
						verified := verifiedFixture(t, key.Root)
						state, err := prior.state.Update(t.Context(), differences(prior.Entries, verified))
						if err != nil {
							t.Fatal(err)
						}
						payload, err := store.encode(t.Context(), key, verified, state, prior, true)
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
						written, replaced, err := store.save(t.Context(), cache, key, verified, state, prior)
						if err != nil || written == 0 || replaced != (excess > 0) {
							t.Fatalf("publication: bytes=%d replaced=%v err=%v", written, replaced, err)
						}
						loaded, _ := loadFramedFixture(t, root, cache, store)
						wantRecords := 1 - excess
						if loaded.journalRecords != wantRecords {
							t.Fatalf("journal records=%d, want %d", loaded.journalRecords, wantRecords)
						}
						got := framedOracle(t, root, cache, store)
						if got.Written || got.Hashed != 0 || got.JournalRecordsLoaded != wantRecords {
							t.Fatalf("published state not reusable: %+v", got)
						}
					})
				}
			}
		})
	}
}

func TestDerivedRecoveryDiscardsDamagedFrameAndFollowingTransactions(t *testing.T) {
	for _, store := range []framedStore{derivedStore(true), observationJournalStore()} {
		t.Run(store.name, func(t *testing.T) {

			for _, damage := range []string{"checksum", "truncated-payload"} {
				t.Run(damage, func(t *testing.T) {
					root, cache := fixture(t)
					framedOracle(t, root, cache, store)
					if err := os.WriteFile(filepath.Join(root, "a"), []byte("first"), 0600); err != nil {
						t.Fatal(err)
					}
					prefix := framedOracle(t, root, cache, store)
					loaded, key := loadFramedFixture(t, root, cache, store)
					badFrame := loaded.diskSize
					for _, edit := range []struct{ path, body string }{{"a", "second"}, {"b", "third"}} {
						if err := os.WriteFile(filepath.Join(root, edit.path), []byte(edit.body), 0600); err != nil {
							t.Fatal(err)
						}
						framedOracle(t, root, cache, store)
					}
					file := filepath.Join(cache, store.name)
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
					recovered, status, _, err := store.load(t.Context(), cache, key)
					if err != nil || status != "recovered" || recovered.journalRecords != 1 {
						t.Fatalf("recovery: status=%s records=%d err=%v", status, recovered.journalRecords, err)
					}
					if diff, err := prefix.State.Diff(t.Context(), recovered.state); err != nil || len(diff) != 0 {
						t.Fatalf("did not recover exact valid prefix: %v %v", diff, err)
					}
					got := framedOracle(t, root, cache, store)
					if !got.Written || !got.ReplacedBase || got.JournalRecordsLoaded != 1 || got.Hashed != 2 || got.Reused != 1 {
						t.Fatalf("recovery did not refresh source and replace base: %+v", got)
					}
					got = framedOracle(t, root, cache, store)
					if got.Written || got.Hashed != 0 || got.JournalRecordsLoaded != 0 {
						t.Fatalf("recovered base is not reusable: %+v", got)
					}
				})
			}
		})
	}
}

// Equal length is not a generation identity. Collect two real verified base
// publications of the same size, allowing native stamp encoding widths to vary.
func TestFramedRejectsEqualSizedDifferentGeneration(t *testing.T) {
	for _, store := range []framedStore{derivedStore(true), observationJournalStore()} {
		t.Run(store.name, func(t *testing.T) {
			root, cache := fixture(t)
			replacement := store
			replacement.journal = false
			seen := make(map[int]loadedCheckpoint)
			for i := 0; i < 16; i++ {
				if err := os.WriteFile(filepath.Join(root, "a"), []byte{byte('A' + i)}, 0600); err != nil {
					t.Fatal(err)
				}
				current := framedOracle(t, root, cache, replacement)
				loaded, key := loadFramedFixture(t, root, cache, store)
				old, found := seen[loaded.diskSize]
				if !found {
					seen[loaded.diskSize] = loaded
					continue
				}
				if old.diskDigest == loaded.diskDigest {
					t.Fatal("fixture did not change generation")
				}
				file := filepath.Join(cache, store.name)
				before, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				verified := verifiedFixture(t, key.Root)
				if _, _, err := store.save(t.Context(), cache, key, verified, current.State, old); err == nil {
					t.Fatal("accepted stale append after equal-sized replacement")
				}
				after, err := os.ReadFile(file)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("stale append changed cache")
				}
				if got := framedOracle(t, root, cache, store); got.Written || got.Hashed != 0 {
					t.Fatalf("replacement no longer reusable: %+v", got)
				}
				return
			}
			t.Fatal("could not construct equal-sized valid generations")
		})
	}
}
