package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
)

// watchPreparation contains observed source metadata, never destination state.
// Native hints identify which observations to refresh. Every shipped body still
// has to be verified against its manifest by the normal snapshot packer.
type watchPreparation struct {
	manifest proto.Manifest
	gi       GitInfo
	policy   proto.SelectionPolicy
	evidence *explicitSelectionEvidence
	entries  map[string]int
	fullAt   time.Time
}

// explicitSelectionEvidence proves that a previously enumerated selection is
// still authorized without enumerating every entry again. Directory identity,
// native ctime, mtime and modes detect structural changes independently of event
// delivery; fresh policy contents determine exclusions. This optimization is
// deliberately unavailable for Git-driven selection or unsupported stat types.
type explicitSelectionEvidence struct {
	root        string
	opts        SelectOptions
	directories map[string]fs.FileInfo
	ignore      []byte
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
	if builder == nil {
		return manifest, gi, policy, nil, fmt.Errorf("watch snapshot requires a builder")
	}
	info, statErr := os.Lstat(s.root)
	if statErr != nil || !os.SameFile(s.identity, info) {
		return manifest, gi, policy, nil, fmt.Errorf("watched checkout was removed or replaced")
	}
	s.dirtyMu.Lock()
	dirty, full, reset := s.dirty, s.fullScan, s.resetHashes
	s.dirty, s.fullScan, s.resetHashes = nil, false, false
	s.dirtyMu.Unlock()
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
	if prior == nil || prior.evidence == nil || time.Since(prior.fullAt) > 30*time.Second {
		full = true
	}
	if !full && prior.evidence.verifySelection() != nil {
		full = true
	}
	var changed []string
	if !full {
		for name := range dirty {
			rel, e := filepath.Rel(s.root, name)
			if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				full = true
				break
			}
			rel = filepath.ToSlash(rel)
			index, ok := prior.entries[rel]
			if !ok || prior.manifest.Entries[index].Type == proto.EntryDir {
				full = true
				break
			}
			changed = append(changed, rel)
		}
	}
	if full {
		return s.prepareFull(builder)
	}
	manifest, err = builder.update(s.root, prior.manifest, changed)
	if err != nil {
		return manifest, gi, policy, nil, fmt.Errorf("refreshing watched source: %w", err)
	}
	if err = prior.evidence.verify(); err != nil {
		return manifest, gi, policy, nil, err
	}
	next := *prior
	next.manifest = manifest
	s.prepared = &next
	guard = &SelectionGuard{root: s.root, identity: s.identity, explicit: prior.evidence}
	return cloneManifest(manifest), prior.gi, clonePolicy(prior.policy), guard, nil
}

func (s *Watch) prepareFull(builder *Builder) (proto.Manifest, GitInfo, proto.SelectionPolicy, *SelectionGuard, error) {
	paths, gi, policy, guard, err := SelectFilesGuarded(s.root, s.opts)
	if err != nil {
		return proto.Manifest{}, gi, policy, nil, fmt.Errorf("selecting watched source: %w", err)
	}
	guard.identity = s.identity
	manifest, err := builder.Build(s.root, paths)
	if err != nil {
		return manifest, gi, policy, nil, fmt.Errorf("building watched source: %w", err)
	}
	evidence, err := captureExplicitSelection(s.root, s.opts, manifest, gi, policy)
	if err != nil {
		return manifest, gi, policy, nil, err
	}
	// Capture directory stamps before re-enumerating selection, then check the
	// same stamps afterward: changes between enumeration and capture cannot seed
	// an apparently valid cache with an omitted or newly excluded path.
	if err := guard.Verify(); err != nil {
		return manifest, gi, policy, nil, err
	}
	if evidence != nil {
		if err := evidence.verify(); err != nil {
			return manifest, gi, policy, nil, err
		}
		guard.explicit = evidence
	}
	entries := make(map[string]int, len(manifest.Entries))
	for i, e := range manifest.Entries {
		entries[e.Path] = i
	}
	s.prepared = &watchPreparation{manifest: manifest, gi: gi, policy: clonePolicy(policy), evidence: evidence, entries: entries, fullAt: time.Now()}
	return cloneManifest(manifest), gi, clonePolicy(policy), guard, nil
}

func captureExplicitSelection(root string, opts SelectOptions, m proto.Manifest, gi GitInfo, policy proto.SelectionPolicy) (*explicitSelectionEvidence, error) {
	data, err := os.ReadFile(filepath.Join(root, ".errandignore"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !slices.Equal(policyLines(data), policy.Ignore) {
		return nil, fmt.Errorf("snapshot: explicit policy changed during preparation; retry")
	}
	directories := map[string]fs.FileInfo{}
	names := map[string]bool{".": true}
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
			return nil, fmt.Errorf("snapshot: selected directory %q was replaced", name)
		}
		if _, _, ok := changeStamp(info); !ok {
			return nil, nil
		}
		directories[name] = info
	}
	opts.Caches = slices.Clone(opts.Caches)
	return &explicitSelectionEvidence{root: root, opts: opts, directories: directories, ignore: data, gi: gi}, nil
}

func (e *explicitSelectionEvidence) verify() error {
	if err := e.verifySelection(); err != nil {
		return err
	}
	// Check metadata after source work and again after freezing, not in the
	// preliminary selection-only check as well.
	gi, _ := gitInfo(e.root)
	if gi != e.gi {
		return fmt.Errorf("snapshot: repository metadata changed after manifest construction; retry")
	}
	return e.verifyPolicy()
}

func (e *explicitSelectionEvidence) verifyPolicy() error {
	data, err := os.ReadFile(filepath.Join(e.root, ".errandignore"))
	if err != nil || !bytes.Equal(data, e.ignore) {
		return fmt.Errorf("snapshot: selection policy changed after manifest construction; retry")
	}
	return nil
}

func (e *explicitSelectionEvidence) verifySelection() error {
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
		if err != nil || !sameDirectoryEvidence(old, info) {
			return fmt.Errorf("snapshot: selected directory %q changed after manifest construction; retry", name)
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

func cloneManifest(m proto.Manifest) proto.Manifest {
	return proto.Manifest{Entries: slices.Clone(m.Entries)}
}
func clonePolicy(p proto.SelectionPolicy) proto.SelectionPolicy {
	p.Ignore = slices.Clone(p.Ignore)
	p.Caches = slices.Clone(p.Caches)
	p.Artifacts = slices.Clone(p.Artifacts)
	return p
}

// update preserves prior observations of untouched entries. It does not certify
// them as current bodies: Pack still verifies anything later selected for upload.
func (b *Builder) update(root string, prior proto.Manifest, paths []string) (proto.Manifest, error) {
	b.next = make(map[string]fileHash, len(b.hashes))
	for name, h := range b.hashes {
		b.next[name] = h
	}
	part, err := buildBoundedContext(context.Background(), root, paths, -1, -1, b)
	if err != nil {
		b.next = nil
		return proto.Manifest{}, err
	}
	replacements := make(map[string]proto.ManifestEntry, len(part.Entries))
	for _, e := range part.Entries {
		replacements[e.Path] = e
	}
	result := cloneManifest(prior)
	for i, e := range result.Entries {
		if next, ok := replacements[e.Path]; ok {
			result.Entries[i] = next
		}
	}
	b.hashes, b.next = b.next, nil
	return result, nil
}
