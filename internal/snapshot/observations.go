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
	Observations   []Observation
	Hashed, Reused int
	Changed        bool
	collect        bool
	prior          []Observation
	cursor         int
	currentStamp   ObservationStamp
	pending        []int
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
		r.prior = opts.Prior
		r.Changed = paths.Len() != len(opts.Prior)
		r.Observations = make([]Observation, 0, paths.Len())
	}
	m, err := buildSelectedContext(ctx, root, paths.paths, opts.MaxBytes, opts.MaxEntries, nil, r)
	r.Manifest = m
	r.prior = nil
	return r, err
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
		if old.Entry.Path == name && old.Stamp == s {
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
		r.pending = append(r.pending, len(r.Observations))
	}
	r.Observations = append(r.Observations, Observation{entry, r.currentStamp})
}

// Verify binds newly read metadata/hashes to their pre-build observations.
// Unchanged regular files need only the fresh stat done by the builder. Ignored
// sibling churn can change directory timestamps without changing its metadata.
func (r *ObservedBuild) Verify(ctx context.Context, root string) error {
	for _, i := range r.pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := &r.Observations[i]
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(e.Entry.Path)))
		if err != nil {
			return err
		}
		after, err := Fingerprint(info)
		if err != nil {
			return err
		}
		if !matchesPrepared(e.Stamp, after, e.Entry) {
			return fmt.Errorf("source changed while preparing %q", e.Entry.Path)
		}
		if e.Stamp != after {
			r.Changed = true
		}
		e.Stamp = after
	}
	return ctx.Err()
}

func matchesPrepared(before, after ObservationStamp, entry proto.ManifestEntry) bool {
	if entry.Type != proto.EntryDir {
		return before == after
	}
	return fs.FileMode(before.Mode).IsDir() && fs.FileMode(after.Mode).IsDir() &&
		before.Device == after.Device && before.Inode == after.Inode &&
		before.BornSec == after.BornSec && before.BornNS == after.BornNS &&
		before.Mode == after.Mode && entry.Mode == uint32(fs.FileMode(after.Mode).Perm())
}
