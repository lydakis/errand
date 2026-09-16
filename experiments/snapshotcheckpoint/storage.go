//go:build darwin || linux

package snapshotcheckpoint

import (
	"context"

	"github.com/lydakis/errand/internal/manifest"
	"github.com/lydakis/errand/internal/snapshot"
)

// Storage owns format and publication policy. Preparation consumes validated
// observations and optional prior metadata without interpreting format flags.
type checkpointStorage interface {
	load(context.Context, string, identity) (loadedCheckpoint, string, int64, error)
	save(context.Context, string, identity, snapshot.VerifiedObservations, *manifest.Snapshot, loadedCheckpoint) (int64, bool, error)
}

type observationStore struct{ direct bool }

func (s observationStore) load(ctx context.Context, dir string, key identity) (loadedCheckpoint, string, int64, error) {
	return readCheckpoint(ctx, dir, key, !s.direct)
}

func (observationStore) save(ctx context.Context, dir string, key identity, entries snapshot.VerifiedObservations, _ *manifest.Snapshot, _ loadedCheckpoint) (int64, bool, error) {
	n, err := writeCheckpoint(ctx, dir, key, entries)
	return n, false, err
}

type framedStore struct {
	name, magic    string
	index, journal bool
}

func derivedStore(journal bool) framedStore {
	return framedStore{name: "index", magic: derivedMagic, index: true, journal: journal}
}

func observationJournalStore() framedStore {
	return framedStore{name: "observations", magic: "ERRAND-OBS-JOURNAL-1\n", journal: true}
}

// PrepareObservationJournal stores only observations and changed records. Every
// process reconstructs the selected adaptive index; no derived nodes are on disk.
func PrepareObservationJournal(ctx context.Context, root, cache string, opts snapshot.SelectOptions) (Result, error) {
	config := defaultPreparation(false)
	config.store = observationJournalStore()
	return prepare(ctx, root, cache, opts, config)
}
