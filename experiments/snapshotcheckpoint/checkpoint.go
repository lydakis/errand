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
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/manifest"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

type Phases struct{ Selection, Load, Scan, Hash, Index, Verify, Save time.Duration }
type Result struct {
	State                   *manifest.Snapshot
	Phases                  Phases
	Reused, Hashed          int
	CheckpointBytes         int64
	Written                 bool
	CacheStatus, CacheError string
}

type identity struct {
	Root, OS, Boot, Filesystem string
	Device, Inode              uint64
	Policy                     [32]byte
}
type observation struct {
	Entry proto.ManifestEntry
	Stamp stamp
}
type checkpoint struct {
	Identity identity
	Entries  []observation
}

// Cold is the f459bb2 selection/build/index path, shared with the comparator.
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

// Prepare reopens a bounded checkpoint and checks every selected path. Changed
// paths use the existing snapshot builder; updates use the existing adaptive
// manifest engine. Callers must still freeze/verify shipped bodies normally.
func Prepare(ctx context.Context, root, cache string, opts snapshot.SelectOptions) (r Result, err error) {
	return prepare(ctx, root, cache, opts, snapshot.BuildBoundedContext)
}

// A per-call builder boundary lets tests schedule real source changes between
// construction and verification without global hooks or timing assumptions.
func prepare(ctx context.Context, root, cache string, opts snapshot.SelectOptions, build func(context.Context, string, []string, int64, int) (proto.Manifest, error)) (r Result, err error) {
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
	r.Phases.Selection = time.Since(start)
	start = time.Now()
	key, err := checkoutIdentity(root, policy, opts)
	if err != nil {
		cold, coldErr := Cold(ctx, root, opts)
		cold.CacheStatus, cold.CacheError = "unsupported", err.Error()
		return cold, coldErr
	}
	prior, status, size, err := readCheckpoint(ctx, cache, key)
	r.CacheStatus, r.CheckpointBytes = status, size
	r.Phases.Load = time.Since(start)
	if err != nil {
		return r, err
	}
	start = time.Now()
	selected := make(map[string]bool, len(paths))
	for _, name := range paths {
		selected[name] = true
		for p := path.Dir(name); p != "." && p != "/"; p = path.Dir(p) {
			selected[p] = true
		}
	}
	paths = make([]string, 0, len(selected))
	for name := range selected {
		paths = append(paths, name)
	}
	slices.Sort(paths)
	next := checkpoint{Identity: key, Entries: make([]observation, len(paths))}
	positions := make(map[string]int, len(paths))
	var changed []string
	writable := status != "hit" || len(paths) != len(prior.Entries)
	oldIndex := 0
	for i, name := range paths {
		if err = ctx.Err(); err != nil {
			return r, err
		}
		info, e := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if e != nil {
			return r, e
		}
		stamp, e := fingerprint(info)
		if e != nil {
			return r, e
		}
		positions[name] = i
		next.Entries[i].Stamp = stamp
		for oldIndex < len(prior.Entries) && prior.Entries[oldIndex].Entry.Path < name {
			oldIndex++
		}
		if oldIndex < len(prior.Entries) && prior.Entries[oldIndex].Entry.Path == name && prior.Entries[oldIndex].Stamp == stamp {
			next.Entries[i].Entry = prior.Entries[oldIndex].Entry
			if info.Mode().IsRegular() {
				r.Reused++
			}
		} else {
			changed = append(changed, name)
			writable = true
			if info.Mode().IsRegular() {
				r.Hashed++
			}
		}
	}
	r.Phases.Scan = time.Since(start)
	start = time.Now()
	part, err := build(ctx, root, changed, -1, -1)
	if err != nil {
		return r, err
	}
	for _, entry := range part.Entries {
		i, ok := positions[entry.Path]
		if !ok {
			return r, fmt.Errorf("builder returned unselected path %q", entry.Path)
		}
		info, e := os.Lstat(filepath.Join(root, filepath.FromSlash(entry.Path)))
		if e != nil {
			return r, e
		}
		after, e := fingerprint(info)
		if e != nil || !matchesPrepared(next.Entries[i].Stamp, after, entry) {
			return r, fmt.Errorf("source changed while preparing %q", entry.Path)
		}
		next.Entries[i].Entry = entry
		next.Entries[i].Stamp = after
	}
	r.Phases.Hash = time.Since(start)
	start = time.Now()
	if status == "hit" {
		r.State, err = stateOf(ctx, prior)
		if err == nil {
			edits := differences(prior.Entries, next.Entries)
			if len(edits) != 0 {
				r.State, err = r.State.Update(ctx, edits)
			}
		}
	} else {
		r.State, err = stateOf(ctx, next)
	}
	r.Phases.Index = time.Since(start)
	if err != nil {
		return r, err
	}
	start = time.Now()
	if err = guard.Verify(); err != nil {
		return r, err
	}
	now, err := checkoutIdentity(root, policy, opts)
	if err != nil || key != now {
		return r, fmt.Errorf("checkout identity changed during preparation")
	}
	if err = ctx.Err(); err != nil {
		return r, err
	}
	r.Phases.Verify = time.Since(start)
	if writable {
		start = time.Now()
		r.CheckpointBytes, err = writeCheckpoint(ctx, cache, next)
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
	s, err := fingerprint(info)
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

func stateOf(ctx context.Context, cp checkpoint) (*manifest.Snapshot, error) {
	// The cold builder emits nil for an empty selection. Preserve that wire
	// representation: JSON null and [] have different root hashes.
	var m proto.Manifest
	if len(cp.Entries) != 0 {
		m.Entries = make([]proto.ManifestEntry, len(cp.Entries))
	}
	for i, e := range cp.Entries {
		m.Entries[i] = e.Entry
	}
	s, err := manifest.New(ctx, m)
	if err == nil {
		err = s.PrepareUpdates(ctx)
	}
	return s, err
}

func differences(before, after []observation) []manifest.Edit {
	var edits []manifest.Edit
	i, j := 0, 0
	for i < len(before) || j < len(after) {
		if i < len(before) && (j == len(after) || before[i].Entry.Path < after[j].Entry.Path) {
			edits = append(edits, manifest.Edit{Entry: before[i].Entry, Delete: true})
			i++
		} else if j < len(after) && (i == len(before) || after[j].Entry.Path < before[i].Entry.Path) {
			edits = append(edits, manifest.Edit{Entry: after[j].Entry})
			j++
		} else {
			if before[i].Entry != after[j].Entry {
				edits = append(edits, manifest.Edit{Entry: after[j].Entry})
			}
			i++
			j++
		}
	}
	return edits
}
