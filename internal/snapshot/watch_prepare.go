package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	manifeststate "github.com/lydakis/errand/internal/manifest"
	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
)

// traceWatch logs each preparation's mode and fallback reason for benchmarks.
var traceWatch = os.Getenv("ERRAND_TRACE_WATCH") == "1"

// watchPreparation contains observed source metadata, never destination state.
// Native hints identify which observations to refresh. Every shipped body still
// has to be verified against its manifest by the normal snapshot packer.
type watchPreparation struct {
	state    *manifeststate.Snapshot
	gi       GitInfo
	policy   proto.SelectionPolicy
	evidence *selectionEvidence
	fullAt   time.Time
}

// selectionEvidence proves that a previously enumerated selection is
// still authorized without enumerating every entry again. Directory identity,
// native ctime, mtime and modes detect structural changes independently of event
// delivery; fresh policy contents determine exclusions. Git-driven selection
// also binds the tracked set and every ignore/config source Git reads (see
// gitSelectionEvidence). Unsupported stat types and non-Git recursive
// selection have no evidence and always use full selection.
type selectionEvidence struct {
	root        string
	opts        SelectOptions
	directories map[string]fs.FileInfo
	ignore      []byte                // explicit .errandignore; unused for Git
	git         *gitSelectionEvidence // nil for explicit selection
	gi          GitInfo
}

// Prepare refreshes a source snapshot for a serialized watch session. Ordinary
// writes under an explicit .errandignore refresh only hinted files. First use,
// structural/control events, overflow, changed selection evidence, and expired
// preparations use full selection. Events arriving during preparation remain
// pending for the next cycle. A failed preparation forces full reconciliation.
// Expiry is checked on preparation; it does not schedule work while idle.
// The returned guard must still be verified after freezing the source.
func (s *Watch) Prepare(builder *Builder) (manifest proto.Manifest, gi GitInfo, policy proto.SelectionPolicy, guard *SelectionGuard, err error) {
	state, gi, policy, guard, err := s.PrepareSnapshot(builder)
	if err != nil {
		return manifest, gi, policy, guard, err
	}
	manifest, err = state.Manifest(context.Background())
	return
}

// PrepareSnapshot retains the immutable index across transfer preparation.
// Callers export metadata only at a wire or durable-record boundary.
func (s *Watch) PrepareSnapshot(builder *Builder) (state *manifeststate.Snapshot, gi GitInfo, policy proto.SelectionPolicy, guard *SelectionGuard, err error) {
	if builder == nil {
		return nil, gi, policy, nil, fmt.Errorf("watch snapshot requires a builder")
	}
	info, statErr := os.Lstat(s.root)
	if statErr != nil || !os.SameFile(s.identity, info) {
		return nil, gi, policy, nil, fmt.Errorf("watched checkout was removed or replaced")
	}
	s.dirtyMu.Lock()
	dirty, full, reset := s.dirty, s.fullScan, s.resetHashes
	s.dirty, s.fullScan, s.resetHashes = nil, false, false
	s.dirtyMu.Unlock()
	reason := "incremental"
	switch {
	case reset:
		reason = "invalidated"
	case full:
		reason = "structural"
	}
	if traceWatch {
		started := time.Now()
		defer func() {
			mode := "incremental"
			if full {
				mode = "full"
			}
			entries := 0
			if state != nil {
				entries = state.Len()
			}
			log.Printf("errand watch: prepare=%s reason=%s dirty=%d entries=%d elapsed_us=%d err=%t",
				mode, reason, len(dirty), entries, time.Since(started).Microseconds(), err != nil)
		}()
	}
	defer func() {
		if err != nil {
			s.InvalidatePreparation()
		}
	}()
	prior := s.prepared
	if reset {
		builder.hashes = nil
	} else {
		for name, structural := range dirty {
			delete(builder.hashes, name)
			if structural {
				for cached := range builder.hashes {
					if strings.HasPrefix(cached, name+string(filepath.Separator)) {
						delete(builder.hashes, cached)
					}
				}
			}
		}
	}
	if !full {
		switch {
		case prior == nil:
			full, reason = true, "first"
		case prior.evidence == nil:
			full, reason = true, "no-evidence"
		case time.Since(prior.fullAt) > 30*time.Second:
			full, reason = true, "expired"
		}
	}
	if !full && prior.evidence.verifySelection() != nil {
		full, reason = true, "evidence-changed"
	}
	var changed []string
	if !full {
		for name := range dirty {
			rel, e := filepath.Rel(s.root, name)
			if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				full, reason = true, "outside-root"
				break
			}
			rel = filepath.ToSlash(rel)
			entry, ok := prior.state.Lookup(rel)
			if !ok || entry.Type == proto.EntryDir {
				full, reason = true, "unknown-path"
				break
			}
			changed = append(changed, rel)
		}
	}
	if full {
		return s.prepareFull(builder)
	}
	state, updateErr := builder.update(s.root, prior.state, changed)
	err = updateErr
	if err != nil {
		return nil, gi, policy, nil, fmt.Errorf("refreshing watched source: %w", err)
	}
	if err = prior.evidence.verify(); err != nil {
		return nil, gi, policy, nil, err
	}
	next := *prior
	next.state = state
	s.prepared = &next
	guard = &SelectionGuard{root: s.root, identity: s.identity, evidence: prior.evidence}
	return state, prior.gi, clonePolicy(prior.policy), guard, nil
}

func (s *Watch) prepareFull(builder *Builder) (*manifeststate.Snapshot, GitInfo, proto.SelectionPolicy, *SelectionGuard, error) {
	paths, gi, policy, guard, err := SelectFilesGuarded(s.root, s.opts)
	if err != nil {
		return nil, gi, policy, nil, fmt.Errorf("selecting watched source: %w", err)
	}
	guard.identity = s.identity
	manifest, err := builder.Build(s.root, paths)
	if err != nil {
		return nil, gi, policy, nil, fmt.Errorf("building watched source: %w", err)
	}
	evidence, err := captureSelectionEvidence(s.root, s.opts, manifest, gi, policy)
	if err != nil {
		return nil, gi, policy, nil, err
	}
	// Capture directory stamps before re-enumerating selection, then check the
	// same stamps afterward: changes between enumeration and capture cannot seed
	// an apparently valid cache with an omitted or newly excluded path.
	if err := guard.Verify(); err != nil {
		return nil, gi, policy, nil, err
	}
	if evidence != nil {
		if err := evidence.verify(); err != nil {
			return nil, gi, policy, nil, err
		}
		guard.evidence = evidence
	}
	state, err := manifeststate.New(context.Background(), manifest)
	if err != nil {
		return nil, gi, policy, nil, err
	}
	if evidence != nil {
		if err := state.PrepareUpdates(context.Background()); err != nil {
			return nil, gi, policy, nil, err
		}
	}
	s.prepared = &watchPreparation{state: state, gi: gi, policy: clonePolicy(policy), evidence: evidence, fullAt: time.Now()}
	return state, gi, clonePolicy(policy), guard, nil
}

func captureSelectionEvidence(root string, opts SelectOptions, m proto.Manifest, gi GitInfo, policy proto.SelectionPolicy) (*selectionEvidence, error) {
	data, err := os.ReadFile(filepath.Join(root, ".errandignore"))
	var git *gitSelectionEvidence
	var names map[string]bool
	if os.IsNotExist(err) {
		if !gi.Repository {
			return nil, nil
		}
		// Unignored directories come from the walk; ancestors of tracked files
		// inside ignored directories are added below from the manifest.
		git, names, err = captureGitSelection(root, opts)
		if err != nil || git == nil {
			return nil, err
		}
		data = nil
	} else if err != nil {
		return nil, err
	} else if !slices.Equal(policyLines(data), policy.Ignore) {
		return nil, sourceChangedf("snapshot: explicit policy changed during preparation; retry")
	} else {
		names = map[string]bool{}
	}
	directories := map[string]fs.FileInfo{}
	names["."] = true
	for _, e := range m.Entries {
		if e.Type == proto.EntryDir {
			names[e.Path] = true
		}
		for p := path.Dir(e.Path); p != "."; p = path.Dir(p) {
			names[p] = true
		}
	}
	for name := range names {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, sourceChangedf("snapshot: selected directory %q was replaced", name)
		}
		if _, _, ok := changeStamp(info); !ok {
			return nil, nil
		}
		directories[name] = info
	}
	opts.Caches = slices.Clone(opts.Caches)
	return &selectionEvidence{root: root, opts: opts, directories: directories, ignore: data, git: git, gi: gi}, nil
}

func (e *selectionEvidence) verify() error {
	if err := e.verifySelection(); err != nil {
		return err
	}
	// Check metadata after source work and again after freezing, not in the
	// preliminary selection-only check as well.
	gi, _ := gitInfo(e.root)
	if gi != e.gi {
		return sourceChangedf("snapshot: repository metadata changed after manifest construction; retry")
	}
	return e.verifyPolicy()
}

func (e *selectionEvidence) verifyPolicy() error {
	if e.git != nil {
		return e.git.verify(e.root)
	}
	data, err := os.ReadFile(filepath.Join(e.root, ".errandignore"))
	if err != nil {
		return sourceReadError(err)
	}
	if !bytes.Equal(data, e.ignore) {
		return sourceChangedf("snapshot: selection policy changed after manifest construction; retry")
	}
	return nil
}

func (e *selectionEvidence) verifySelection() error {
	if err := validateSnapshotRoot(e.root, e.opts); err != nil {
		return err
	}
	if err := pathpolicy.ValidateCacheCasing(e.root, e.opts.Caches); err != nil {
		return err
	}
	if err := e.verifyPolicy(); err != nil {
		return err
	}
	for name, old := range e.directories {
		info, err := os.Lstat(filepath.Join(e.root, filepath.FromSlash(name)))
		if err != nil {
			return sourceReadError(err)
		}
		if !sameDirectoryEvidence(old, info) {
			return sourceChangedf("snapshot: selected directory %q changed after manifest construction; retry", name)
		}
	}
	// Match full selection's before/after policy check. An in-place policy
	// write does not change its parent directory's structural stamp.
	return e.verifyPolicy()
}

func sameDirectoryEvidence(old, now fs.FileInfo) bool {
	if now == nil || !now.IsDir() || !os.SameFile(old, now) || old.Mode() != now.Mode() || !old.ModTime().Equal(now.ModTime()) {
		return false
	}
	a, b, ok := changeStamp(old)
	c, d, supported := changeStamp(now)
	return ok && supported && a == c && b == d
}

func clonePolicy(p proto.SelectionPolicy) proto.SelectionPolicy {
	p.Ignore = slices.Clone(p.Ignore)
	p.Caches = slices.Clone(p.Caches)
	p.Artifacts = slices.Clone(p.Artifacts)
	return p
}

// update preserves prior observations of untouched entries. It does not certify
// them as current bodies: Pack still verifies anything later selected for upload.
func (b *Builder) update(root string, prior *manifeststate.Snapshot, paths []string) (*manifeststate.Snapshot, error) {
	// Hash evidence is advisory. Commit only the refreshed entries after metadata
	// validation succeeds; untouched evidence does not need a full map copy.
	b.next = make(map[string]fileHash, len(paths))
	part, err := buildBoundedContext(context.Background(), root, paths, -1, -1, b)
	if err != nil {
		b.next = nil
		return nil, err
	}
	edits := make([]manifeststate.Edit, 0, len(part.Entries))
	for _, e := range part.Entries {
		if _, ok := prior.Lookup(e.Path); ok {
			edits = append(edits, manifeststate.Edit{Entry: e})
		}
	}
	next, err := prior.Update(context.Background(), edits)
	if err != nil {
		b.next = nil
		return nil, err
	}
	if b.hashes == nil {
		b.hashes = make(map[string]fileHash)
	}
	for name, hash := range b.next {
		b.hashes[name] = hash
	}
	b.next = nil
	return next, nil
}
