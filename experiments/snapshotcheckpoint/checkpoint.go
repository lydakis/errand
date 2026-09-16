//go:build darwin || linux

// Package snapshotcheckpoint is an opt-in experiment, not a production cache.
// Selection and stat work is fresh on every call. Checkpoints contain advisory
// observations, never authorization, destination state or verified file bodies.
package snapshotcheckpoint

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/manifest"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

// Scan remains zero for the shared pass; retained frozen reports used it separately.
type Phases struct{ Selection, Load, Scan, Hash, Index, Verify, Save time.Duration }
type Result struct {
	State          *manifest.Snapshot
	Phases         Phases
	Reused, Hashed int
	// CheckpointBytes counts published bytes when Written is true; otherwise
	// it is the loaded file size on a hit. Failed writes may report partial bytes.
	CheckpointBytes int64
	Written         bool
	// ReplacedBase reports replacement of an existing framed base, including
	// ordinary derived-mode rewrites, journal compaction and suffix recovery.
	ReplacedBase bool
	// JournalRecordsLoaded counts transactions replayed before publication.
	// It excludes a new append and remains the pre-compaction count on replacement.
	JournalRecordsLoaded    int
	CacheStatus, CacheError string
}

type identity struct {
	Root, OS, Boot, Filesystem string
	Device, Inode              uint64
	Policy                     [32]byte
}
type observation = snapshot.Observation
type checkpoint struct {
	Identity identity
	Entries  []observation
}

// Cold uses ordinary selection/build/index without checkpoint observations.
// It deliberately includes PrepareUpdates so both paths return a ready index.
func Cold(ctx context.Context, root string, opts snapshot.SelectOptions) (r Result, err error) {
	start := time.Now()
	if err = ctx.Err(); err != nil {
		return
	}
	paths, _, _, guard, err := snapshot.SelectFilesGuarded(root, opts)
	r.Phases.Selection = time.Since(start)
	if err != nil {
		return r, err
	}
	start = time.Now()
	m, err := snapshot.BuildBoundedContext(ctx, root, paths, -1, -1)
	r.Phases.Hash = time.Since(start)
	if err != nil {
		return r, err
	}
	for _, e := range m.Entries {
		if e.Type == proto.EntryFile {
			r.Hashed++
		}
	}
	start = time.Now()
	r.State, err = manifest.New(ctx, m)
	if err == nil {
		err = r.State.PrepareUpdates(ctx)
	}
	r.Phases.Index = time.Since(start)
	start = time.Now()
	if err == nil {
		err = guard.Verify()
	}
	if err == nil {
		err = ctx.Err()
	}
	r.Phases.Verify = time.Since(start)
	return r, err
}

// Prepare reopens a bounded checkpoint and builds all selected metadata through
// the shared builder. Callers must still freeze/verify shipped bodies normally.
func Prepare(ctx context.Context, root, cache string, opts snapshot.SelectOptions) (Result, error) {
	return prepare(ctx, root, cache, opts, defaultPreparation(false))
}

// PrepareCurrent compares direct current-state construction against restoring
// prior state and applying edits. Both return the same ready adaptive index.
func PrepareCurrent(ctx context.Context, root, cache string, opts snapshot.SelectOptions) (Result, error) {
	return prepare(ctx, root, cache, opts, defaultPreparation(true))
}

// PrepareDerived compares full derived-index replacement and bounded journal
// publication with the observation-only control. Neither is a production cache.
func PrepareDerived(ctx context.Context, root, cache string, opts snapshot.SelectOptions, journal bool) (Result, error) {
	config := defaultPreparation(false)
	config.store = derivedStore(journal)
	return prepare(ctx, root, cache, opts, config)
}

type preparation struct {
	store      checkpointStorage
	entryLimit int
	identify   func(string, proto.SelectionPolicy, snapshot.SelectOptions) (identity, error)
	build      func(context.Context, string, snapshot.BuildPaths, snapshot.ObservationOptions) (*snapshot.ObservedBuild, error)
}

func defaultPreparation(direct bool) preparation {
	return preparation{store: observationStore{direct: direct}, entryLimit: maxEntries, identify: checkoutIdentity, build: snapshot.BuildObservedContext}
}

// Per-call dependencies let tests exercise admission boundaries and schedule
// source changes without global hooks or timing assumptions.
func prepare(ctx context.Context, root, cache string, opts snapshot.SelectOptions, config preparation) (r Result, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return
	}
	cache, err = filepath.Abs(cache)
	if err != nil {
		return
	}
	// Resolve existing ancestors even when the requested cache does not exist.
	parent := cache
	var missing []string
	for {
		resolved, e := filepath.EvalSymlinks(parent)
		if e == nil {
			cache = filepath.Join(append([]string{resolved}, missing...)...)
			break
		}
		if !os.IsNotExist(e) {
			return r, e
		}
		missing = append([]string{filepath.Base(parent)}, missing...)
		parent = filepath.Dir(parent)
	}
	if rel, e := filepath.Rel(root, cache); e != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return r, fmt.Errorf("checkpoint directory must be outside source")
	}
	// Lexical paths do not identify case aliases on APFS. Check existing
	// ancestors by native identity before any cache file can enter selection.
	rootInfo, err := os.Stat(root)
	if err != nil {
		return r, err
	}
	for ancestor := cache; ; ancestor = filepath.Dir(ancestor) {
		info, e := os.Stat(ancestor)
		if e != nil && !os.IsNotExist(e) {
			return r, e
		}
		if e == nil && os.SameFile(rootInfo, info) {
			return r, fmt.Errorf("checkpoint directory must be outside source")
		}
		if ancestor == filepath.Dir(ancestor) {
			break
		}
	}
	start := time.Now()
	paths, _, policy, guard, err := snapshot.SelectFilesGuarded(root, opts)
	if err != nil {
		return r, err
	}
	preparedPaths := snapshot.PrepareBuildPaths(paths)
	r.Phases.Selection = time.Since(start)
	start = time.Now()
	key, keyErr := config.identify(root, policy, opts)
	var prior loadedCheckpoint
	collect := keyErr == nil && preparedPaths.Len() <= config.entryLimit
	switch {
	case keyErr != nil:
		r.CacheStatus, r.CacheError = "unsupported", keyErr.Error()
	case !collect:
		r.CacheStatus = "oversized"
	default:
		prior, r.CacheStatus, r.CheckpointBytes, err = config.store.load(ctx, cache, key)
	}
	r.Phases.Load = time.Since(start)
	if err != nil {
		return r, err
	}
	start = time.Now()
	built, err := config.build(ctx, root, preparedPaths, snapshot.ObservationOptions{
		Prior: prior.Entries, Collect: collect, MaxBytes: -1, MaxEntries: -1,
	})
	if err != nil {
		return r, err
	}
	r.Hashed, r.Reused = built.Hashed, built.Reused
	var verified snapshot.VerifiedObservations
	if collect {
		verified, err = built.Verify(ctx)
		if err != nil {
			return r, err
		}
	}
	writable := collect && (r.CacheStatus != "hit" || verified.Changed())
	r.Phases.Hash = time.Since(start)
	start = time.Now()
	if prior.state != nil {
		r.State = prior.state
		err = r.State.PrepareUpdates(ctx)
		if err == nil {
			edits := differences(prior.Entries, verified)
			if len(edits) != 0 {
				r.State, err = r.State.Update(ctx, edits)
			}
		}
	} else {
		r.State, err = manifest.New(ctx, built.Manifest)
		if err == nil {
			err = r.State.PrepareUpdates(ctx)
		}
	}
	r.Phases.Index = time.Since(start)
	if err != nil {
		return r, err
	}
	start = time.Now()
	if err = guard.Verify(); err != nil {
		return r, err
	}
	if keyErr == nil {
		now, identityErr := config.identify(root, policy, opts)
		if identityErr != nil || key != now {
			return r, fmt.Errorf("checkout identity changed during preparation")
		}
	}
	if err = ctx.Err(); err != nil {
		return r, err
	}
	r.Phases.Verify = time.Since(start)
	if writable {
		start = time.Now()
		r.CheckpointBytes, r.ReplacedBase, err = config.store.save(ctx, cache, key, verified, r.State, prior)
		r.Phases.Save = time.Since(start)
		if err != nil {
			if ctx.Err() != nil {
				return r, ctx.Err()
			}
			r.CacheError, err = err.Error(), nil // Recomputable cache never blocks a valid snapshot.
		} else {
			r.Written = true
		}
	}
	r.JournalRecordsLoaded = prior.journalRecords
	return r, nil
}

func checkoutIdentity(root string, policy proto.SelectionPolicy, opts snapshot.SelectOptions) (identity, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return identity{}, err
	}
	if !info.IsDir() {
		return identity{}, fmt.Errorf("source root is not a directory")
	}
	s, err := snapshot.Fingerprint(info)
	if err != nil {
		return identity{}, err
	}
	boot, err := bootIdentity()
	if err != nil || boot == "" {
		return identity{}, fmt.Errorf("boot identity unavailable: %v", err)
	}
	filesystem, err := filesystemIdentity(root)
	if err != nil {
		return identity{}, err
	}
	data, err := json.Marshal(struct {
		Policy  proto.SelectionPolicy
		Options snapshot.SelectOptions
	}{policy, opts})
	return identity{Root: root, OS: runtime.GOOS, Boot: boot, Filesystem: filesystem, Device: s.Device, Inode: s.Inode, Policy: sha256.Sum256(data)}, err
}

func differences(before []observation, after snapshot.VerifiedObservations) []manifest.Edit {
	var edits []manifest.Edit
	i, j, count := 0, 0, after.Len()
	for i < len(before) && j < count {
		entry := after.At(j).Entry
		switch {
		case before[i].Entry.Path < entry.Path:
			edits = append(edits, manifest.Edit{Entry: before[i].Entry, Delete: true})
			i++
		case entry.Path < before[i].Entry.Path:
			edits = append(edits, manifest.Edit{Entry: entry})
			j++
		default:
			if before[i].Entry != entry {
				edits = append(edits, manifest.Edit{Entry: entry})
			}
			i++
			j++
		}
	}
	for ; i < len(before); i++ {
		edits = append(edits, manifest.Edit{Entry: before[i].Entry, Delete: true})
	}
	for ; j < count; j++ {
		edits = append(edits, manifest.Edit{Entry: after.At(j).Entry})
	}
	return edits
}
