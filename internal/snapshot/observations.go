package snapshot

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/lydakis/errand/internal/proto"
)

// ObservationStamp is native evidence for advisory hash reuse, not proof of
// current file contents. Callers must bind persisted observations to the
// checkout, boot, filesystem and selection policy, and still verify packed bodies.
type ObservationStamp struct {
	Device, Inode                                  uint64
	Size                                           int64
	Mode                                           uint32
	ModifiedSec, ModifiedNS, ChangedSec, ChangedNS int64
	BornSec, BornNS                                int64
}

type Observation struct {
	Entry proto.ManifestEntry
	Stamp ObservationStamp
}

// BuildPaths owns the sorted selection plus implicit ancestors. Preparing it once
// lets callers decide admission before loading advisory observations.
// It carries no selection authority; callers retain their selection guard.
type BuildPaths struct{ paths []string }

func PrepareBuildPaths(paths []string) BuildPaths { return BuildPaths{expandPaths(paths)} }
func (p BuildPaths) Len() int                     { return len(p.paths) }

type ObservationOptions struct {
	Prior      []Observation
	Collect    bool
	MaxBytes   int64 // Negative means unlimited.
	MaxEntries int   // Negative means unlimited.
}

// ObservedBuild owns a manifest and pending observations from the ordinary
// builder pass. Verify must succeed before observations are persisted. Selection
// verification remains the caller's responsibility, including after freezing.
type ObservedBuild struct {
	Manifest       proto.Manifest
	batch          *pendingObservations
	Hashed, Reused int
	Changed        bool
	collect        bool
	prior          []Observation
	cursor         int
	currentStamp   ObservationStamp
}

// Copies share consumption state. Published batches have no mutable alias
// reachable through a copied build value. Builds are used serially.
type pendingObservations struct {
	entries []Observation
	pending []int
	root    string
	ready   bool
}

// BuildObservedContext accepts sorted, validated, identity-matched advisory
// observations. Callers decide whether to collect before loading a checkpoint.
// Collection disabled builds use ordinary hashing and allocation behavior.
func BuildObservedContext(ctx context.Context, root string, paths BuildPaths, opts ObservationOptions) (*ObservedBuild, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := &ObservedBuild{collect: opts.Collect}
	if opts.Collect {
		absoluteRoot, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		r.prior = opts.Prior
		r.Changed = paths.Len() != len(opts.Prior)
		r.batch = &pendingObservations{entries: make([]Observation, 0, paths.Len()), root: absoluteRoot}
	}
	m, err := buildSelectedContext(ctx, root, paths.paths, opts.MaxBytes, opts.MaxEntries, nil, r)
	r.Manifest = m
	r.prior = nil
	if err != nil {
		return nil, err
	}
	if r.batch != nil {
		r.batch.ready = true
	}
	return r, nil
}

// lookup and record run consecutively for one entry in the serialized builder.
// Retain the stamp here so it is computed once, without returning a large value
// through the ordinary builder's nil-observation path.
func (r *ObservedBuild) lookup(name string, info fs.FileInfo) (*Observation, error) {
	if r == nil || !r.collect {
		return nil, nil
	}
	s, err := Fingerprint(info)
	if err != nil {
		return nil, err
	}
	r.currentStamp = s
	for r.cursor < len(r.prior) && r.prior[r.cursor].Entry.Path < name {
		r.cursor++
	}
	if r.cursor < len(r.prior) {
		old := &r.prior[r.cursor]
		if old.Entry.Path == name && sameObservation(old.Stamp, s) {
			return old, nil
		}
	}
	return nil, nil
}

func (r *ObservedBuild) record(entry proto.ManifestEntry, reused bool) {
	if r == nil {
		return
	}
	if entry.Type == proto.EntryFile {
		if reused {
			r.Reused++
		} else {
			r.Hashed++
		}
	}
	if !r.collect {
		return
	}
	if !reused {
		r.Changed = true
	}
	if !reused || entry.Type == proto.EntryDir {
		r.batch.pending = append(r.batch.pending, len(r.batch.entries))
	}
	r.batch.entries = append(r.batch.entries, Observation{entry, r.currentStamp})
}

// VerifiedObservations is an immutable, root-bound batch of advisory observations.
// Only a completed build followed by successful Verify can produce a valid batch.
// At returns a value: callers cannot mutate the backing observations. Verification
// does not freeze files or authorize selection; guards and packed-body checks remain
// the caller's responsibility.
type VerifiedObservations struct {
	entries []Observation
	root    string
	valid   bool
}

func (v VerifiedObservations) Valid() bool          { return v.valid }
func (v VerifiedObservations) Root() string         { return v.root }
func (v VerifiedObservations) Len() int             { return len(v.entries) }
func (v VerifiedObservations) At(i int) Observation { return v.entries[i] }

// Verify consumes the pending batch, including on failure. It binds newly read
// metadata/hashes to pre-build observations. Unchanged files use the fresh stat
// already done by the builder. Ignored sibling churn may change directory stamps.
func (r *ObservedBuild) Verify(ctx context.Context) (VerifiedObservations, error) {
	if r == nil || r.batch == nil || !r.batch.ready {
		return VerifiedObservations{}, fmt.Errorf("snapshot: no pending observations to verify")
	}
	batch := r.batch
	batch.ready = false
	entries, pending := batch.entries, batch.pending
	batch.entries, batch.pending = nil, nil
	for _, i := range pending {
		if err := ctx.Err(); err != nil {
			return VerifiedObservations{}, err
		}
		e := &entries[i]
		info, err := os.Lstat(filepath.Join(batch.root, filepath.FromSlash(e.Entry.Path)))
		if err != nil {
			return VerifiedObservations{}, err
		}
		after, err := Fingerprint(info)
		if err != nil {
			return VerifiedObservations{}, err
		}
		if !matchesPrepared(e.Stamp, after, e.Entry) {
			return VerifiedObservations{}, fmt.Errorf("source changed while preparing %q", e.Entry.Path)
		}
		if e.Stamp != after {
			r.Changed = true
		}
		e.Stamp = after
	}
	if err := ctx.Err(); err != nil {
		return VerifiedObservations{}, err
	}
	return VerifiedObservations{entries: entries, root: batch.root, valid: true}, nil
}

// sameObservation is the shared advisory reuse rule for native file evidence.
func sameObservation(before, after ObservationStamp) bool { return before == after }

func matchesPrepared(before, after ObservationStamp, entry proto.ManifestEntry) bool {
	if entry.Type != proto.EntryDir {
		return sameObservation(before, after)
	}
	return fs.FileMode(before.Mode).IsDir() && fs.FileMode(after.Mode).IsDir() &&
		before.Device == after.Device && before.Inode == after.Inode &&
		before.BornSec == after.BornSec && before.BornNS == after.BornNS &&
		before.Mode == after.Mode && entry.Mode == uint32(fs.FileMode(after.Mode).Perm())
}
